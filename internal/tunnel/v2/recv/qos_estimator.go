package recv

import (
	"math"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultQoSSustain     = 3 * time.Second
	defaultQoSSampleFloor = 4
	defaultQoSTick        = time.Second
	defaultQoSGroupMature = 1500 * time.Millisecond
	defaultFECHealthAlpha = 0.75
	defaultFECHealthBeta  = 0.05
	qosDecisionSamples    = 3

	qosRateGapEnter = 0.10
	qosRateGapExit  = 0.03
)

type qosConfig struct {
	Sustain     time.Duration
	SampleFloor uint64
	SessionID   uint64
	LaneID      uint8
	Tick        time.Duration
	AutoStart   bool
}

type qosStatus struct {
	UDPLimited      bool
	TCPLimited      bool
	RepairCount     uint8
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
	primarySwitched bool
}

type qosRateSample struct {
	At                 time.Time
	DataKind           transport.Kind
	RepairKind         transport.Kind
	DataBytes          uint64
	RepairBytes        uint64
	ProfileDataBytes   uint64
	ProfileRepairBytes uint64
}

type qosEstimator struct {
	mu             sync.Mutex
	cfg            qosConfig
	emit           func(qosStatus)
	currentPrimary transport.Kind
	currentRole    qosRole
	directions     map[qosDirectionKey]*qosDirectionState
	transports     map[transport.Kind]*qosTransportState
	done           chan struct{}
	stopOnce       sync.Once
	closed         bool
}

type qosDirectionKey struct {
	dataKind   transport.Kind
	repairKind transport.Kind
	role       qosRole
}

type qosRole uint8

const (
	qosRoleData qosRole = iota + 1
	qosRoleShadow
)

type qosDirectionState struct {
	dataKind   transport.Kind
	repairKind transport.Kind
	role       qosRole

	sampleTotal uint64

	lastTickAt           time.Time
	pendingActual        uint64
	pendingRepair        uint64
	pendingProfileData   uint64
	pendingProfileRepair uint64
	lastActual           uint64
	lastRepair           uint64
	lastProfileData      uint64
	lastProfileRepair    uint64
	lastActualBps        uint32
	lastRepairBps        uint32

	dataRateLimited bool
	shadowLimited   bool

	dataRatePending qosPending
	shadowPending   qosPending
}

type qosTransportState struct {
	limited      bool
	deliveredBps uint32
}

type qosPending struct {
	active  bool
	limited bool
	since   time.Time
	bps     uint32
	values  [qosDecisionSamples]float64
	bpsVals [qosDecisionSamples]uint32
	count   int
	next    int
}

type qosLimitStateSource uint8

const (
	qosLimitStateDataRate qosLimitStateSource = iota + 1
	qosLimitStateShadow
)

type qosEstimate struct {
	At          time.Time
	DataKind    transport.Kind
	RepairKind  transport.Kind
	SampleTotal uint64
	ActualBps   uint32
	ExpectedBps uint32
	ShadowBps   uint32
	RateGap     float64
	ShadowGap   float64
}

func newQoSEstimator(cfg qosConfig, emit func(qosStatus)) *qosEstimator {
	if cfg.Sustain <= 0 {
		cfg.Sustain = defaultQoSSustain
	}
	if cfg.SampleFloor == 0 {
		cfg.SampleFloor = defaultQoSSampleFloor
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultQoSTick
	}
	e := &qosEstimator{
		cfg:            cfg,
		emit:           emit,
		currentPrimary: transport.KindUDP,
		currentRole:    qosRoleData,
		directions:     make(map[qosDirectionKey]*qosDirectionState),
		transports:     make(map[transport.Kind]*qosTransportState),
		done:           make(chan struct{}),
	}
	if cfg.AutoStart {
		go e.run()
	}
	return e
}

func (e *qosEstimator) Close() {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() {
		close(e.done)
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
	})
}

func (e *qosEstimator) run() {
	ticker := time.NewTicker(e.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			e.emitStatuses(e.Tick(now))
		case <-e.done:
			return
		}
	}
}

func (e *qosEstimator) ObserveRate(sample qosRateSample) []qosStatus {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	statuses := e.observeRateLocked(sample)
	e.mu.Unlock()
	return statuses
}

func (e *qosEstimator) observeRateLocked(sample qosRateSample) []qosStatus {
	if !validQoSDirection(sample.DataKind, sample.RepairKind) {
		return nil
	}
	now := normalizeQoSTime(sample.At)
	if sample.DataKind != e.currentPrimary || sample.RepairKind != otherTransportKind(e.currentPrimary) {
		return nil
	}
	state := e.direction(sample.DataKind, sample.RepairKind, e.currentRoleLocked())
	state.observeRate(sample, now)
	return nil
}

func (e *qosEstimator) Tick(now time.Time) []qosStatus {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	statuses := e.tickLocked(now)
	e.mu.Unlock()
	return statuses
}

func (e *qosEstimator) tickLocked(now time.Time) []qosStatus {
	now = normalizeQoSTime(now)
	if !qosKnownKind(e.currentPrimary) {
		return nil
	}
	state := e.direction(e.currentPrimary, otherTransportKind(e.currentPrimary), e.currentRoleLocked())
	if !state.flush(now, e.cfg.Tick) {
		return nil
	}
	if status, ok := e.evaluateDirection(state, now); ok {
		return []qosStatus{status}
	}
	return nil
}

func (e *qosEstimator) evaluateDirection(state *qosDirectionState, now time.Time) (qosStatus, bool) {
	rateReady := state.sampleTotal >= e.cfg.SampleFloor && state.hasProfileTick()
	var estimate qosEstimate
	if rateReady {
		estimate = state.estimate(now)
		if debuglog.Enabled() {
			debuglog.Printf("recv/qos_rate", "estimate session=%d lane=%d role=%s primary=%s shadow=%s samples=%d profile_observed=%t last_actual=%d last_repair=%d last_profile_data=%d last_profile_repair=%d raw_actual_bps=%d raw_repair_bps=%d shadow_base=%.0f shadow_equiv=%.0f actual_bps=%d expected_bps=%d shadow_bps=%d rate_gap=%.3f shadow_gap=%.3f",
				e.cfg.SessionID, e.cfg.LaneID,
				qosRoleLabel(state.role),
				kindMetricLabel(estimate.DataKind), kindMetricLabel(estimate.RepairKind),
				estimate.SampleTotal, state.hasProfileTick(),
				state.lastActual, state.lastRepair, state.lastProfileData, state.lastProfileRepair,
				state.lastActualBps, state.lastRepairBps,
				state.shadowBaseRate(),
				state.shadowEquivalentRate(),
				estimate.ActualBps, estimate.ExpectedBps, estimate.ShadowBps,
				estimate.RateGap, estimate.ShadowGap)
		}
		e.setDeliveredBps(estimate.DataKind, estimate.ActualBps)
		if estimate.ShadowBps > 0 {
			e.setDeliveredBps(estimate.RepairKind, estimate.ShadowBps)
		}
		e.recordEstimate(estimate)
		if debuglog.Enabled() {
			debuglog.Printf("recv/qos", "estimate session=%d lane=%d role=%s primary=%s shadow=%s samples=%d last_actual=%d last_repair=%d actual_bps=%d expected_bps=%d shadow_bps=%d rate_gap=%.3f shadow_gap=%.3f",
				e.cfg.SessionID, e.cfg.LaneID,
				qosRoleLabel(state.role),
				kindMetricLabel(estimate.DataKind), kindMetricLabel(estimate.RepairKind),
				estimate.SampleTotal, state.lastActual, state.lastRepair,
				estimate.ActualBps, estimate.ExpectedBps, estimate.ShadowBps,
				estimate.RateGap, estimate.ShadowGap)
		}
		if status, ok := e.evaluateRateState(state, estimate, now); ok {
			return status, true
		}
	}

	return qosStatus{}, false
}

func (e *qosEstimator) evaluateRateState(state *qosDirectionState, estimate qosEstimate, now time.Time) (qosStatus, bool) {
	var (
		changed bool
		status  qosStatus
	)
	if state.role == qosRoleData && estimate.ExpectedBps > 0 {
		status, changed = e.driveHighGapState(state, qosLimitStateDataRate, estimate.RateGap, estimate.ActualBps, now)
	}
	if changed {
		return status, true
	}

	if state.role == qosRoleShadow && estimate.ActualBps > 0 {
		status, changed = e.driveHighGapState(state, qosLimitStateShadow, estimate.ShadowGap, estimate.ShadowBps, now)
	}
	return status, changed
}

func (e *qosEstimator) currentRoleLocked() qosRole {
	if e.currentRole != qosRoleData && e.currentRole != qosRoleShadow {
		return qosRoleData
	}
	return e.currentRole
}

func (e *qosEstimator) direction(dataKind, repairKind transport.Kind, role qosRole) *qosDirectionState {
	key := qosDirectionKey{dataKind: dataKind, repairKind: repairKind, role: role}
	state := e.directions[key]
	if state == nil {
		state = &qosDirectionState{dataKind: dataKind, repairKind: repairKind, role: role}
		e.directions[key] = state
	}
	return state
}

func (e *qosEstimator) directionIfExists(dataKind, repairKind transport.Kind, role qosRole) *qosDirectionState {
	return e.directions[qosDirectionKey{dataKind: dataKind, repairKind: repairKind, role: role}]
}

func (e *qosEstimator) transport(kind transport.Kind) *qosTransportState {
	state := e.transports[kind]
	if state == nil {
		state = &qosTransportState{}
		e.transports[kind] = state
	}
	return state
}

func (e *qosEstimator) kindLimitedLocked(kind transport.Kind) bool {
	if !qosKnownKind(kind) {
		return false
	}
	for _, direction := range e.directions {
		if direction.dataKind == kind && direction.dataRateLimited {
			return true
		}
		if direction.repairKind == kind && direction.shadowLimited {
			return true
		}
	}
	return false
}

func (e *qosEstimator) setDeliveredBps(kind transport.Kind, bps uint32) {
	e.transport(kind).deliveredBps = bps
}

func (e *qosEstimator) driveLimitState(direction *qosDirectionState, source qosLimitStateSource, limited bool, bps uint32, now time.Time) (qosStatus, bool) {
	if direction == nil {
		return qosStatus{}, false
	}
	kind := direction.limitStateKind(source)
	e.setDeliveredBps(kind, bps)

	if limited {
		if direction.limitState(source) && e.transport(kind).limited {
			direction.clearPending(source)
			return qosStatus{}, false
		}
	} else {
		if source != qosLimitStateShadow && !direction.limitState(source) {
			direction.clearPending(source)
			return qosStatus{}, false
		}
		if source == qosLimitStateShadow && !e.transport(kind).limited {
			direction.clearPending(source)
			return qosStatus{}, false
		}
	}
	pending := direction.pending(source)
	if !pending.active || pending.limited != limited {
		*pending = qosPending{
			active:  true,
			limited: limited,
			since:   now,
			bps:     bps,
		}
		if debuglog.Enabled() {
			debuglog.Printf("recv/qos", "state_pending session=%d lane=%d role=%s primary=%s shadow=%s source=%s target=%s limited=%t bps=%d sustain_ms=%d",
				e.cfg.SessionID, e.cfg.LaneID,
				qosRoleLabel(direction.role),
				kindMetricLabel(direction.dataKind), kindMetricLabel(direction.repairKind),
				qosLimitStateSourceLabel(source), kindMetricLabel(kind), limited, bps,
				e.cfg.Sustain.Milliseconds())
		}
		return qosStatus{}, false
	}
	pending.bps = bps
	if now.Sub(pending.since) < e.cfg.Sustain {
		return qosStatus{}, false
	}
	return e.commitLimitState(direction, source, limited, bps, now, pending.since)
}

func (e *qosEstimator) driveHighGapState(direction *qosDirectionState, source qosLimitStateSource, gap float64, bps uint32, now time.Time) (qosStatus, bool) {
	avg, avgBps, ready := direction.observeDecisionSample(source, gap, bps, now)
	if debuglog.Enabled() {
		pending := direction.pending(source)
		debuglog.Printf("recv/qos_rate", "decision session=%d lane=%d role=%s primary=%s shadow=%s source=%s target=%s value=%.3f bps=%d count=%d next=%d values=%v bps_values=%v avg=%.3f avg_bps=%d ready=%t since_ms=%d",
			e.cfg.SessionID, e.cfg.LaneID,
			qosRoleLabel(direction.role),
			kindMetricLabel(direction.dataKind), kindMetricLabel(direction.repairKind),
			qosLimitStateSourceLabel(source), kindMetricLabel(direction.limitStateKind(source)),
			gap, bps, pending.count, pending.next, pending.values, pending.bpsVals,
			avg, avgBps, ready, now.Sub(pending.since).Milliseconds())
	}
	if !ready {
		return qosStatus{}, false
	}
	since := direction.pending(source).since
	if since.IsZero() {
		since = now
	}
	switch {
	case avg >= qosRateGapEnter:
		return e.commitLimitState(direction, source, true, avgBps, now, since)
	case avg <= qosRateGapExit:
		return e.commitLimitState(direction, source, false, avgBps, now, since)
	default:
		direction.clearPending(source)
		return qosStatus{}, false
	}
}

func (e *qosEstimator) commitLimitState(direction *qosDirectionState, source qosLimitStateSource, limited bool, bps uint32, now, since time.Time) (qosStatus, bool) {
	if direction == nil {
		return qosStatus{}, false
	}
	kind := direction.limitStateKind(source)
	before := e.snapshot()
	e.setDeliveredBps(kind, bps)

	noStateChange := false
	if limited {
		if direction.limitState(source) && e.transport(kind).limited {
			noStateChange = true
		}
	} else {
		if source != qosLimitStateShadow && !direction.limitState(source) {
			noStateChange = true
		}
		if source == qosLimitStateShadow && !e.transport(kind).limited {
			noStateChange = true
		}
	}
	if noStateChange {
		direction.clearPending(source)
		after := e.snapshot()
		if !shouldEmitQoSStatus(before, after) {
			return qosStatus{}, false
		}
		after.primarySwitched = e.applyCommittedPrimaryLocked(after)
		if debuglog.Enabled() {
			debuglog.Printf("recv/qos", "state_update session=%d lane=%d role=%s primary=%s shadow=%s source=%s target=%s limited=%t bps=%d reason=preferred_bps",
				e.cfg.SessionID, e.cfg.LaneID,
				qosRoleLabel(direction.role),
				kindMetricLabel(direction.dataKind), kindMetricLabel(direction.repairKind),
				qosLimitStateSourceLabel(source), kindMetricLabel(kind), limited, bps)
		}
		return after, true
	}
	waited := now.Sub(since)

	if limited {
		direction.setLimitState(source, true)
		e.transport(kind).limited = true
	} else {
		if source == qosLimitStateShadow {
			e.clearLimitStateForKind(kind)
		} else {
			direction.setLimitState(source, false)
		}
		e.transport(kind).limited = e.kindLimitedLocked(kind)
	}
	direction.clearAllPending()
	after := e.snapshot()
	if !shouldEmitQoSStatus(before, after) {
		return qosStatus{}, false
	}
	after.primarySwitched = e.applyCommittedPrimaryLocked(after)
	event := "limited_clear"
	if limited {
		event = "limited_active"
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv/qos", "state_commit session=%d lane=%d role=%s primary=%s shadow=%s source=%s target=%s limited=%t bps=%d waited_ms=%d",
			e.cfg.SessionID, e.cfg.LaneID,
			qosRoleLabel(direction.role),
			kindMetricLabel(direction.dataKind), kindMetricLabel(direction.repairKind),
			qosLimitStateSourceLabel(source), kindMetricLabel(kind), limited, bps,
			waited.Milliseconds())
	}
	e.recordEvent(event, kind)
	return after, true
}

func (e *qosEstimator) applyCommittedPrimaryLocked(status qosStatus) bool {
	next := preferredPrimaryFromQoS(status)
	if next == e.currentPrimary {
		return false
	}
	old := e.currentPrimary
	if debuglog.Enabled() {
		debuglog.Printf("recv/qos", "primary_switch session=%d lane=%d from=%s to=%s",
			e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(old), kindMetricLabel(next))
	}
	if state := e.directionIfExists(next, old, qosRoleData); state != nil {
		state.resetDataRole()
	}
	if state := e.directionIfExists(next, old, qosRoleShadow); state != nil {
		state.resetShadowRole()
	}
	if e.kindLimitedLocked(old) {
		e.currentRole = qosRoleShadow
	} else {
		e.currentRole = qosRoleData
	}
	e.currentPrimary = next
	return true
}

func preferredPrimaryFromQoS(status qosStatus) transport.Kind {
	switch {
	case status.UDPLimited && !status.TCPLimited:
		return transport.KindTCP
	case !status.UDPLimited && status.TCPLimited:
		return transport.KindUDP
	case status.UDPLimited && status.TCPLimited:
		if status.TCPDeliveredBps > status.UDPDeliveredBps {
			return transport.KindTCP
		}
		return transport.KindUDP
	default:
		return transport.KindUDP
	}
}

func (e *qosEstimator) snapshot() qosStatus {
	udp := e.transport(transport.KindUDP)
	tcp := e.transport(transport.KindTCP)
	return qosStatus{
		UDPLimited:      udp.limited,
		TCPLimited:      tcp.limited,
		RepairCount:     1,
		UDPDeliveredBps: udp.deliveredBps,
		TCPDeliveredBps: tcp.deliveredBps,
	}
}

func (e *qosEstimator) snapshotStatus() qosStatus {
	if e == nil {
		return qosStatus{RepairCount: 1}
	}
	e.mu.Lock()
	status := e.snapshot()
	e.mu.Unlock()
	return status
}

func (e *qosEstimator) clearLimitStateForKind(kind transport.Kind) {
	for _, direction := range e.directions {
		if direction.dataKind == kind {
			direction.dataRateLimited = false
		}
		if direction.repairKind == kind {
			direction.shadowLimited = false
		}
		if direction.dataKind == kind || direction.repairKind == kind {
			direction.clearAllPending()
		}
	}
}

func (e *qosEstimator) emitStatuses(statuses []qosStatus) {
	if e.emit != nil {
		for _, status := range statuses {
			e.emit(status)
		}
	}
}

func (s *qosDirectionState) observeRate(sample qosRateSample, now time.Time) {
	s.ensureTickStart(now)
	if sample.DataBytes > 0 || sample.RepairBytes > 0 {
		s.sampleTotal++
	}
	s.pendingActual += sample.DataBytes
	s.pendingRepair += sample.RepairBytes
	s.pendingProfileData += sample.ProfileDataBytes
	s.pendingProfileRepair += sample.ProfileRepairBytes
}

func (s *qosDirectionState) ensureTickStart(now time.Time) {
	if s.lastTickAt.IsZero() && !now.IsZero() {
		s.lastTickAt = now
	}
}

func (s *qosDirectionState) flush(now time.Time, tick time.Duration) bool {
	if now.IsZero() {
		return false
	}
	if tick <= 0 {
		tick = defaultQoSTick
	}
	if s.lastTickAt.IsZero() {
		s.lastTickAt = now
		return false
	}
	if now.Sub(s.lastTickAt) < tick {
		return false
	}
	duration := now.Sub(s.lastTickAt)
	pendingActual := s.pendingActual
	pendingRepair := s.pendingRepair
	pendingProfileData := s.pendingProfileData
	pendingProfileRepair := s.pendingProfileRepair
	s.lastTickAt = now
	s.lastActual = s.pendingActual
	s.lastRepair = s.pendingRepair
	s.lastProfileData = s.pendingProfileData
	s.lastProfileRepair = s.pendingProfileRepair
	s.lastActualBps = deliveredBps(s.pendingActual, duration)
	s.lastRepairBps = deliveredBps(s.pendingRepair, duration)
	if debuglog.Enabled() {
		debuglog.Printf("recv/qos_rate", "flush role=%s primary=%s shadow=%s duration_ms=%d pending_actual=%d pending_repair=%d pending_profile_data=%d pending_profile_repair=%d raw_actual_bps=%d raw_repair_bps=%d",
			qosRoleLabel(s.role),
			kindMetricLabel(s.dataKind), kindMetricLabel(s.repairKind),
			duration.Milliseconds(),
			pendingActual, pendingRepair, pendingProfileData, pendingProfileRepair,
			deliveredBps(pendingActual, duration), deliveredBps(pendingRepair, duration))
	}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	return true
}

func (s *qosDirectionState) hasProfileTick() bool {
	return s.lastProfileData > 0 && s.lastProfileRepair > 0
}

func (s *qosDirectionState) estimate(now time.Time) qosEstimate {
	actual := float64(s.lastActualBps)
	shadow := s.shadowEquivalentRate()
	expected := shadow
	expectedRepair := s.expectedRepairRate(actual)
	return qosEstimate{
		At:          now,
		DataKind:    s.dataKind,
		RepairKind:  s.repairKind,
		SampleTotal: s.sampleTotal,
		ActualBps:   clampUint32Float(actual),
		ExpectedBps: clampUint32Float(expected),
		ShadowBps:   clampUint32Float(shadow),
		RateGap:     rateGapRatio(expected, actual),
		ShadowGap:   rateGapRatio(expectedRepair, float64(s.lastRepairBps)),
	}
}

func (s *qosDirectionState) shadowBaseRate() float64 {
	if s.lastRepairBps == 0 || s.lastProfileData == 0 || s.lastProfileRepair == 0 {
		return 0
	}
	return float64(s.lastRepairBps) * float64(s.lastProfileData) / float64(s.lastProfileRepair)
}

func (s *qosDirectionState) shadowEquivalentRate() float64 {
	return s.shadowBaseRate()
}

func (s *qosDirectionState) expectedRepairRate(dataRate float64) float64 {
	if dataRate <= 0 || s.lastProfileData == 0 || s.lastProfileRepair == 0 {
		return 0
	}
	return dataRate * float64(s.lastProfileRepair) / float64(s.lastProfileData)
}

func (s *qosDirectionState) limited() bool {
	return s.dataRateLimited || s.shadowLimited
}

func (s *qosDirectionState) limitState(source qosLimitStateSource) bool {
	switch source {
	case qosLimitStateDataRate:
		return s.dataRateLimited
	case qosLimitStateShadow:
		return s.shadowLimited
	default:
		return false
	}
}

func (s *qosDirectionState) setLimitState(source qosLimitStateSource, limited bool) {
	switch source {
	case qosLimitStateDataRate:
		s.dataRateLimited = limited
	case qosLimitStateShadow:
		s.shadowLimited = limited
	}
}

func (s *qosDirectionState) pending(source qosLimitStateSource) *qosPending {
	switch source {
	case qosLimitStateDataRate:
		return &s.dataRatePending
	case qosLimitStateShadow:
		return &s.shadowPending
	default:
		return &s.dataRatePending
	}
}

func (s *qosDirectionState) observeDecisionSample(source qosLimitStateSource, value float64, bps uint32, now time.Time) (float64, uint32, bool) {
	pending := s.pending(source)
	if pending.count == 0 {
		pending.since = now
	}
	pending.values[pending.next] = value
	pending.bpsVals[pending.next] = bps
	pending.next = (pending.next + 1) % qosDecisionSamples
	if pending.count < qosDecisionSamples {
		pending.count++
	}
	if pending.count < qosDecisionSamples {
		return 0, 0, false
	}
	var (
		sum    float64
		bpsSum uint64
	)
	for i := 0; i < pending.count; i++ {
		sum += pending.values[i]
		bpsSum += uint64(pending.bpsVals[i])
	}
	return sum / float64(pending.count), uint32(bpsSum / uint64(pending.count)), true
}

func (p *qosPending) average() (float64, bool) {
	if p == nil || p.count == 0 {
		return 0, false
	}
	var sum float64
	for i := 0; i < p.count; i++ {
		sum += p.values[i]
	}
	return sum / float64(p.count), true
}

func (p *qosPending) highGapPending() bool {
	avg, ok := p.average()
	return ok && avg >= qosRateGapEnter
}

func (s *qosDirectionState) clearPending(source qosLimitStateSource) {
	*s.pending(source) = qosPending{}
}

func (s *qosDirectionState) clearAllPending() {
	s.dataRatePending = qosPending{}
	s.shadowPending = qosPending{}
}

func (s *qosDirectionState) resetDataRole() {
	s.sampleTotal = 0
	s.lastTickAt = time.Time{}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	s.lastActual = 0
	s.lastRepair = 0
	s.lastProfileData = 0
	s.lastProfileRepair = 0
	s.lastActualBps = 0
	s.lastRepairBps = 0
	s.dataRatePending = qosPending{}
}

func (s *qosDirectionState) resetShadowRole() {
	s.sampleTotal = 0
	s.lastTickAt = time.Time{}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	s.lastActual = 0
	s.lastRepair = 0
	s.lastProfileData = 0
	s.lastProfileRepair = 0
	s.lastActualBps = 0
	s.lastRepairBps = 0
	s.shadowPending = qosPending{}
}

func (s *qosDirectionState) limitStateKind(source qosLimitStateSource) transport.Kind {
	switch source {
	case qosLimitStateDataRate:
		return s.dataKind
	case qosLimitStateShadow:
		return s.repairKind
	default:
		return 0
	}
}

func validQoSDirection(dataKind, repairKind transport.Kind) bool {
	if !qosKnownKind(dataKind) || !qosKnownKind(repairKind) {
		return false
	}
	return dataKind != repairKind
}

func qosKnownKind(kind transport.Kind) bool {
	return kind == transport.KindUDP || kind == transport.KindTCP
}

func otherTransportKind(kind transport.Kind) transport.Kind {
	if kind == transport.KindUDP {
		return transport.KindTCP
	}
	return transport.KindUDP
}

func normalizeQoSTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func deliveredBps(bytes uint64, duration time.Duration) uint32 {
	if duration <= 0 || bytes == 0 {
		return 0
	}
	bps := float64(bytes) * 8 * float64(time.Second) / float64(duration)
	return clampUint32Float(bps)
}

func rateGapRatio(expected, actual float64) float64 {
	if expected <= 0 {
		return 0
	}
	if actual >= expected {
		return 0
	}
	return (expected - actual) / expected
}

func emaUpdate(current, sample, alpha float64) float64 {
	return current*(1-alpha) + sample*alpha
}

func clampUint32Float(v float64) uint32 {
	if v <= 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func sameQoSLimitState(a, b qosStatus) bool {
	return a.UDPLimited == b.UDPLimited &&
		a.TCPLimited == b.TCPLimited
}

func shouldEmitQoSStatus(before, after qosStatus) bool {
	if !sameQoSLimitState(before, after) {
		return true
	}
	if before.UDPLimited && before.TCPLimited &&
		preferredPrimaryFromQoS(before) != preferredPrimaryFromQoS(after) {
		return true
	}
	return false
}

func qosLimitStateSourceLabel(source qosLimitStateSource) string {
	switch source {
	case qosLimitStateDataRate:
		return "data_rate"
	case qosLimitStateShadow:
		return "shadow"
	default:
		return "unknown"
	}
}

func qosRoleLabel(role qosRole) string {
	switch role {
	case qosRoleData:
		return "data"
	case qosRoleShadow:
		return "shadow"
	default:
		return "unknown"
	}
}

func (e *qosEstimator) recordEstimate(sample qosEstimate) {
	if e == nil || e.cfg.SessionID == 0 {
		return
	}
	metrics.SetGauge(metrics.QoSDeliveredBps, float64(sample.ActualBps),
		metrics.L("session", e.cfg.SessionID),
		metrics.L("lane", e.cfg.LaneID),
		metrics.LStr("leg", kindMetricLabel(sample.DataKind)),
	)
	if sample.ShadowBps > 0 {
		metrics.SetGauge(metrics.QoSDeliveredBps, float64(sample.ShadowBps),
			metrics.L("session", e.cfg.SessionID),
			metrics.L("lane", e.cfg.LaneID),
			metrics.LStr("leg", kindMetricLabel(sample.RepairKind)),
		)
	}
}

func (e *qosEstimator) recordEvent(event string, kind transport.Kind) {
	if e == nil {
		return
	}
	if e.cfg.SessionID != 0 {
		metrics.IncCounter(metrics.QoSEventsTotal,
			metrics.LStr("event", event),
			metrics.L("session", e.cfg.SessionID),
			metrics.L("lane", e.cfg.LaneID),
			metrics.LStr("leg", kindMetricLabel(kind)),
		)
	}
	eventlog.Printf("qos_state", "event=%s session=%d lane=%d leg=%s",
		event, e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(kind))
}
