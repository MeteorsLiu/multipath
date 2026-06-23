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
	defaultQoSAlpha       = 0.10
	defaultQoSTick        = time.Second
	defaultQoSGroupMature = 1500 * time.Millisecond
	qosDecisionSamples    = 3

	qosRateGapEnter = 0.10
	qosRateGapExit  = 0.03

	qosFECHealthEnter = 0.70
	qosFECHealthExit  = 0.90

	qosShadowPIDGate          = 0.05
	qosShadowPIDWarmupSamples = 20
	qosShadowPIDKp            = 0.90
	qosShadowPIDKi            = 0.02
	qosShadowPIDKd            = 0.05
	qosShadowPIDLeak          = 0.985
)

type qosConfig struct {
	Sustain     time.Duration
	SampleFloor uint64
	SessionID   uint64
	LaneID      uint8
	Tick        time.Duration
	MatureAfter time.Duration
	Mature      func(rxGroupKey, time.Time) rxWindowResult
	AutoStart   bool
}

type qosStatus struct {
	UDPLimited      bool
	TCPLimited      bool
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
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

type qosHealthSample struct {
	At           time.Time
	DataKind     transport.Kind
	RepairKind   transport.Kind
	DataArrived  uint64
	DataExpected uint64
	DeliveredBps uint32
}

type qosEstimator struct {
	mu             sync.Mutex
	cfg            qosConfig
	emit           func(qosStatus)
	currentPrimary transport.Kind
	currentRole    qosRole
	directions     map[qosDirectionKey]*qosDirectionState
	transports     map[transport.Kind]*qosTransportState
	mature         map[rxGroupKey]*time.Timer
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

	actual qosRateEMA
	repair qosRateEMA

	groupBytes  qosValueEMA
	repairBytes qosValueEMA

	lastTickAt           time.Time
	pendingActual        uint64
	pendingRepair        uint64
	pendingProfileData   uint64
	pendingProfileRepair uint64
	lastActual           uint64
	lastRepair           uint64
	lastProfileData      uint64
	lastProfileRepair    uint64

	pidCorrection float64
	pidIntegral   float64
	pidPrevError  float64

	dataRateLimited bool
	shadowLimited   bool
	healthLimited   bool

	dataRatePending qosPending
	shadowPending   qosPending
	healthPending   qosPending

	health qosHealthState
}

type qosRateEMA struct {
	initialized bool
	value       float64
}

type qosValueEMA struct {
	initialized bool
	value       float64
}

type qosHealthState struct {
	initialized bool
	score       float64
	sampleTotal uint64
	updated     bool
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
	qosLimitStateHealth
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
	if cfg.MatureAfter <= 0 {
		cfg.MatureAfter = defaultQoSGroupMature
	}
	e := &qosEstimator{
		cfg:            cfg,
		emit:           emit,
		currentPrimary: transport.KindUDP,
		currentRole:    qosRoleData,
		directions:     make(map[qosDirectionKey]*qosDirectionState),
		transports:     make(map[transport.Kind]*qosTransportState),
		mature:         make(map[rxGroupKey]*time.Timer),
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
		for key, timer := range e.mature {
			if timer != nil {
				timer.Stop()
			}
			delete(e.mature, key)
		}
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

func (e *qosEstimator) ObserveResult(result rxWindowResult) []qosStatus {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	if result.hasComplete {
		e.cancelMatureLocked(result.completeGroup)
	}
	if result.hasMatureGroup {
		e.armMatureLocked(result.matureGroup)
	}
	var statuses []qosStatus
	for _, sample := range result.rates {
		statuses = append(statuses, e.observeRateLocked(sample)...)
	}
	for _, sample := range result.health {
		e.observeHealthLocked(sample)
	}
	e.mu.Unlock()
	return statuses
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

func (e *qosEstimator) ObserveHealth(sample qosHealthSample) (qosStatus, bool) {
	if !validQoSDirection(sample.DataKind, sample.RepairKind) || sample.DataExpected == 0 {
		return qosStatus{}, false
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return qosStatus{}, false
	}
	e.observeHealthLocked(sample)
	e.mu.Unlock()
	return qosStatus{}, false
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

func (e *qosEstimator) observeHealthLocked(sample qosHealthSample) {
	if !validQoSDirection(sample.DataKind, sample.RepairKind) || sample.DataExpected == 0 {
		return
	}
	now := normalizeQoSTime(sample.At)
	if sample.DataKind != e.currentPrimary || sample.RepairKind != otherTransportKind(e.currentPrimary) {
		return
	}
	if e.currentRoleLocked() != qosRoleData {
		return
	}
	direction := e.direction(sample.DataKind, sample.RepairKind, qosRoleData)
	direction.ensureTickStart(now)
	score := fecHealthScore(sample.DataArrived, sample.DataExpected)
	direction.health.observe(score, sample.DataExpected)
}

func (e *qosEstimator) armMatureLocked(group rxGroupKey) {
	if e.cfg.Mature == nil {
		return
	}
	if timer := e.mature[group]; timer != nil {
		timer.Stop()
	}
	e.mature[group] = time.AfterFunc(e.cfg.MatureAfter, func() {
		e.fireMature(group)
	})
}

func (e *qosEstimator) cancelMatureLocked(group rxGroupKey) {
	if timer := e.mature[group]; timer != nil {
		timer.Stop()
	}
	delete(e.mature, group)
}

func (e *qosEstimator) cancelAllMatureLocked() {
	for group, timer := range e.mature {
		if timer != nil {
			timer.Stop()
		}
		delete(e.mature, group)
	}
}

func (e *qosEstimator) fireMature(group rxGroupKey) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if _, ok := e.mature[group]; !ok {
		e.mu.Unlock()
		return
	}
	delete(e.mature, group)
	mature := e.cfg.Mature
	e.mu.Unlock()

	if mature == nil {
		return
	}
	result := mature(group, time.Now())
	e.emitStatuses(e.ObserveResult(result))
}

func (e *qosEstimator) evaluateDirection(state *qosDirectionState, now time.Time) (qosStatus, bool) {
	rateReady := state.sampleTotal >= e.cfg.SampleFloor && (state.actual.initialized || state.repair.initialized)
	var estimate qosEstimate
	if rateReady {
		profileObserved := false
		if state.hasProfileTick() {
			state.observeProfile()
			profileObserved = true
		} else if state.hasPairedTick() && !state.profileInitialized() {
			state.observeProfile()
			profileObserved = true
		}
		estimate = state.estimate(now)
		if state.hasPairedTick() && !state.limited() && state.canTrainPID(estimate) {
			if !profileObserved {
				state.observeProfile()
			}
			state.trainPID()
			estimate = state.estimate(now)
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

	if state.role == qosRoleData {
		return e.evaluateHealthState(state, estimate, rateReady, now)
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

func (e *qosEstimator) evaluateHealthState(state *qosDirectionState, estimate qosEstimate, rateReady bool, now time.Time) (qosStatus, bool) {
	if state.health.sampleTotal < e.cfg.SampleFloor || !state.health.updated {
		return qosStatus{}, false
	}
	state.health.updated = false
	bps := e.transport(state.dataKind).deliveredBps
	return e.driveLowScoreState(state, qosLimitStateHealth, state.health.score, bps, now)
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
		if direction.dataKind == kind && (direction.dataRateLimited || direction.healthLimited) {
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
	if !ready {
		return qosStatus{}, false
	}
	switch {
	case avg >= qosRateGapEnter:
		return e.commitLimitState(direction, source, true, avgBps, now, now)
	case avg <= qosRateGapExit:
		return e.commitLimitState(direction, source, false, avgBps, now, now)
	default:
		direction.clearPending(source)
		return qosStatus{}, false
	}
}

func (e *qosEstimator) driveLowScoreState(direction *qosDirectionState, source qosLimitStateSource, score float64, bps uint32, now time.Time) (qosStatus, bool) {
	avg, avgBps, ready := direction.observeDecisionSample(source, score, bps, now)
	if !ready {
		return qosStatus{}, false
	}
	switch {
	case avg <= qosFECHealthEnter:
		return e.commitLimitState(direction, source, true, avgBps, now, now)
	case avg >= qosFECHealthExit:
		return e.commitLimitState(direction, source, false, avgBps, now, now)
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
		e.applyCommittedPrimaryLocked(after)
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
	e.applyCommittedPrimaryLocked(after)
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

func (e *qosEstimator) applyCommittedPrimaryLocked(status qosStatus) {
	next := preferredPrimaryFromQoS(status)
	if next == e.currentPrimary {
		return
	}
	old := e.currentPrimary
	if debuglog.Enabled() {
		debuglog.Printf("recv/qos", "primary_switch session=%d lane=%d from=%s to=%s",
			e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(old), kindMetricLabel(next))
	}
	e.cancelAllMatureLocked()
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
		UDPDeliveredBps: udp.deliveredBps,
		TCPDeliveredBps: tcp.deliveredBps,
	}
}

func (e *qosEstimator) clearLimitStateForKind(kind transport.Kind) {
	for _, direction := range e.directions {
		if direction.dataKind == kind {
			direction.dataRateLimited = false
			direction.healthLimited = false
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
	s.lastTickAt = now
	s.lastActual = s.pendingActual
	s.lastRepair = s.pendingRepair
	s.lastProfileData = s.pendingProfileData
	s.lastProfileRepair = s.pendingProfileRepair
	if s.pendingActual > 0 || s.actual.initialized {
		s.actual.observeBytes(s.pendingActual, duration, true)
	}
	if s.pendingRepair > 0 || s.repair.initialized {
		s.repair.observeBytes(s.pendingRepair, duration, true)
	}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	return true
}

func (s *qosDirectionState) hasPairedTick() bool {
	return s.lastActual > 0 && s.lastRepair > 0
}

func (s *qosDirectionState) hasProfileTick() bool {
	return s.lastProfileData > 0 && s.lastProfileRepair > 0
}

func (s *qosDirectionState) profileInitialized() bool {
	return s.groupBytes.initialized && s.repairBytes.initialized
}

func (s *qosDirectionState) observeProfile() {
	dataBytes := s.lastProfileData
	repairBytes := s.lastProfileRepair
	if dataBytes == 0 || repairBytes == 0 {
		dataBytes = s.lastActual
		repairBytes = s.lastRepair
	}
	if dataBytes == 0 || repairBytes == 0 {
		return
	}
	s.groupBytes.observe(float64(dataBytes))
	s.repairBytes.observe(float64(repairBytes))
}

func (s *qosDirectionState) canTrainPID(estimate qosEstimate) bool {
	return estimate.RateGap <= qosShadowPIDGate && estimate.ShadowGap <= qosShadowPIDGate
}

func (s *qosDirectionState) estimate(now time.Time) qosEstimate {
	actual := s.actual.valueOrZero()
	shadow := s.shadowEquivalentRate()
	var expected float64
	if shadow > 0 {
		expected = shadow
	}
	return qosEstimate{
		At:          now,
		DataKind:    s.dataKind,
		RepairKind:  s.repairKind,
		SampleTotal: s.sampleTotal,
		ActualBps:   clampUint32Float(actual),
		ExpectedBps: clampUint32Float(expected),
		ShadowBps:   clampUint32Float(shadow),
		RateGap:     rateGapRatio(expected, actual),
		ShadowGap:   rateGapRatio(actual, shadow),
	}
}

func (s *qosDirectionState) shadowBaseRate() float64 {
	if !s.repair.initialized || !s.groupBytes.initialized || !s.repairBytes.initialized || s.repairBytes.value <= 0 {
		return 0
	}
	return s.repair.value * s.groupBytes.value / s.repairBytes.value
}

func (s *qosDirectionState) shadowEquivalentRate() float64 {
	base := s.shadowBaseRate()
	if base <= 0 {
		return 0
	}
	estimate := base + s.pidCorrection
	if estimate < 0 {
		return 0
	}
	limit := 4 * s.repair.value
	if limit > 0 && estimate > limit {
		return limit
	}
	return estimate
}

func (s *qosDirectionState) trainPID() {
	target := s.actual.valueOrZero()
	if target <= 0 {
		return
	}
	predicted := s.shadowEquivalentRate()
	if predicted <= 0 {
		return
	}
	if rateGapRatio(predicted, target) > qosShadowPIDGate {
		return
	}
	err := target - predicted
	s.pidIntegral = qosShadowPIDLeak*s.pidIntegral + err
	derivative := err - s.pidPrevError
	s.pidPrevError = err
	s.pidCorrection = qosShadowPIDLeak*s.pidCorrection +
		qosShadowPIDKp*err +
		qosShadowPIDKi*s.pidIntegral +
		qosShadowPIDKd*derivative
}

func (r *qosRateEMA) observeBytes(bytes uint64, duration time.Duration, allowInitialZero bool) bool {
	if duration <= 0 {
		return false
	}
	if !r.initialized && bytes == 0 && !allowInitialZero {
		return false
	}
	bps := float64(deliveredBps(bytes, duration))
	r.observeBps(bps)
	return true
}

func (r *qosRateEMA) observeBps(bps float64) {
	if !r.initialized {
		r.initialized = true
		r.value = bps
		return
	}
	r.value = emaUpdate(r.value, bps, defaultQoSAlpha)
}

func (r qosRateEMA) valueOrZero() float64 {
	if !r.initialized {
		return 0
	}
	return r.value
}

func (v *qosValueEMA) observe(sample float64) {
	if !v.initialized {
		v.initialized = true
		v.value = sample
		return
	}
	v.value = emaUpdate(v.value, sample, defaultQoSAlpha)
}

func (s *qosHealthState) observe(score float64, expected uint64) {
	s.sampleTotal += expected
	s.updated = true
	if !s.initialized {
		s.initialized = true
		s.score = score
		return
	}
	s.score = emaUpdate(s.score, score, defaultQoSAlpha)
}

func (s *qosDirectionState) limited() bool {
	return s.dataRateLimited || s.shadowLimited || s.healthLimited
}

func (s *qosDirectionState) limitState(source qosLimitStateSource) bool {
	switch source {
	case qosLimitStateDataRate:
		return s.dataRateLimited
	case qosLimitStateShadow:
		return s.shadowLimited
	case qosLimitStateHealth:
		return s.healthLimited
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
	case qosLimitStateHealth:
		s.healthLimited = limited
	}
}

func (s *qosDirectionState) pending(source qosLimitStateSource) *qosPending {
	switch source {
	case qosLimitStateDataRate:
		return &s.dataRatePending
	case qosLimitStateShadow:
		return &s.shadowPending
	case qosLimitStateHealth:
		return &s.healthPending
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

func (p *qosPending) lowScorePending() bool {
	avg, ok := p.average()
	return ok && avg <= qosFECHealthEnter
}

func (s *qosDirectionState) clearPending(source qosLimitStateSource) {
	*s.pending(source) = qosPending{}
}

func (s *qosDirectionState) clearAllPending() {
	s.dataRatePending = qosPending{}
	s.shadowPending = qosPending{}
	s.healthPending = qosPending{}
}

func (s *qosDirectionState) resetDataRole() {
	s.sampleTotal = 0
	s.actual = qosRateEMA{}
	s.repair = qosRateEMA{}
	s.lastTickAt = time.Time{}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	s.lastActual = 0
	s.lastRepair = 0
	s.lastProfileData = 0
	s.lastProfileRepair = 0
	s.dataRatePending = qosPending{}
	s.healthPending = qosPending{}
	s.health = qosHealthState{}
}

func (s *qosDirectionState) resetShadowRole() {
	s.sampleTotal = 0
	s.actual = qosRateEMA{}
	s.repair = qosRateEMA{}
	s.lastTickAt = time.Time{}
	s.pendingActual = 0
	s.pendingRepair = 0
	s.pendingProfileData = 0
	s.pendingProfileRepair = 0
	s.lastActual = 0
	s.lastRepair = 0
	s.lastProfileData = 0
	s.lastProfileRepair = 0
	s.shadowPending = qosPending{}
}

func (s *qosDirectionState) limitStateKind(source qosLimitStateSource) transport.Kind {
	switch source {
	case qosLimitStateDataRate:
		return s.dataKind
	case qosLimitStateShadow:
		return s.repairKind
	case qosLimitStateHealth:
		return s.dataKind
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

func fecHealthScore(arrived, expected uint64) float64 {
	if expected == 0 {
		return 1
	}
	if arrived > expected {
		arrived = expected
	}
	return float64(arrived) / float64(expected)
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
	case qosLimitStateHealth:
		return "health"
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
