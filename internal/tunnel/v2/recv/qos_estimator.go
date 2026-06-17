package recv

import (
	"math"
	"time"

	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultQoSSustain     = 3 * time.Second
	defaultQoSSampleFloor = 4
	defaultQoSRefresh     = time.Second
	defaultQoSAlpha       = 0.10

	qosRateGapEnter = 0.10
	qosRateGapExit  = 0.03

	qosShadowAdvantageEnter = 1.10
	qosShadowAdvantageExit  = 1.05

	qosShadowPIDGate = 0.05
	qosShadowPIDKp   = 0.90
	qosShadowPIDKi   = 0.02
	qosShadowPIDKd   = 0.05
	qosShadowPIDLeak = 0.985

	qosShadowPIDWarmupSamples = 20
)

type qosConfig struct {
	Sustain     time.Duration
	SampleFloor uint64
	Refresh     time.Duration
	SessionID   uint64
	LaneID      uint8
}

type qosStatus struct {
	Kind         transport.Kind
	Reason       uint8
	DeliveredBps uint32
}

type qosSample struct {
	At             time.Time
	Duration       time.Duration
	DataKind       transport.Kind
	DataArrived    uint64
	DataExpected   uint64
	DataBytes      uint64
	RecoveredBytes uint64
	RepairKind     transport.Kind
	RepairBytes    uint64
}

type qosEstimator struct {
	cfg  qosConfig
	emit func(qosStatus)

	limitedSince map[transport.Kind]time.Time
	lastEmit     map[transport.Kind]map[uint8]time.Time
	ema          map[qosDirectionKey]*qosEMAState
	active       map[transport.Kind]map[uint8]qosActiveEvidence
}

type qosDirectionKey struct {
	dataKind   transport.Kind
	repairKind transport.Kind
}

type qosEstimate struct {
	At           time.Time
	DataKind     transport.Kind
	RepairKind   transport.Kind
	SampleTotal  uint64
	RateGapRatio float64
	ActualBps    uint32
	ExpectedBps  uint32
	ShadowBps    uint32
	DeliveredBps uint32
}

type qosEMAState struct {
	dataKind         transport.Kind
	repairKind       transport.Kind
	initialized      bool
	sampleCount      uint64
	sampleTotal      uint64
	actualBpsEMA     float64
	expectedBpsEMA   float64
	repairBpsEMA     float64
	profileDataEMA   float64
	profileRepairEMA float64
	pidCorrection    float64
	pidIntegral      float64
	pidPrevError     float64
}

type qosActiveEvidence struct {
	lastEvidence time.Time
	status       qosStatus
}

func newQoSEstimator(cfg qosConfig, emit func(qosStatus)) *qosEstimator {
	if cfg.Sustain <= 0 {
		cfg.Sustain = defaultQoSSustain
	}
	if cfg.SampleFloor == 0 {
		cfg.SampleFloor = defaultQoSSampleFloor
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = defaultQoSRefresh
	}
	return &qosEstimator{
		cfg:          cfg,
		emit:         emit,
		limitedSince: make(map[transport.Kind]time.Time),
		lastEmit:     make(map[transport.Kind]map[uint8]time.Time),
		ema:          make(map[qosDirectionKey]*qosEMAState),
		active:       make(map[transport.Kind]map[uint8]qosActiveEvidence),
	}
}

func (e *qosEstimator) Observe(sample qosSample) {
	if !qosKnownKind(sample.DataKind) {
		return
	}
	if !qosKnownKind(sample.RepairKind) || sample.DataKind == sample.RepairKind {
		return
	}
	estimate, ok := e.updateEstimate(sample)
	if !ok {
		return
	}
	for _, status := range e.evaluate(estimate) {
		if e.emit != nil {
			e.emit(status)
		}
	}
}

func (e *qosEstimator) updateEstimate(sample qosSample) (qosEstimate, bool) {
	now := sample.At
	if now.IsZero() {
		now = time.Now()
		sample.At = now
	}
	key := qosDirectionKey{dataKind: sample.DataKind, repairKind: sample.RepairKind}
	state := e.ema[key]
	if state == nil {
		state = &qosEMAState{dataKind: sample.DataKind, repairKind: sample.RepairKind}
		e.ema[key] = state
	}
	state.observe(sample)
	if state.sampleTotal < e.cfg.SampleFloor {
		e.recordEvent("below_floor", sample.DataKind, 0)
		return qosEstimate{}, false
	}
	estimate := state.estimate(now)
	e.recordSampleMetrics(estimate)
	return estimate, true
}

func (e *qosEstimator) evaluate(sample qosEstimate) []qosStatus {
	var out []qosStatus
	now := sample.At
	if now.IsZero() {
		now = time.Now()
	}
	if status, ok := e.evaluateLimited(sample, now); ok {
		if e.shouldEmit(status.Kind, status.Reason, now) {
			e.recordEvent("limited_active", status.Kind, status.Reason)
			out = append(out, status)
		}
	}
	if status, ok := e.evaluateShadowLimited(sample, now); ok {
		if e.shouldEmit(status.Kind, status.Reason, now) {
			e.recordEvent("limited_active", status.Kind, status.Reason)
			out = append(out, status)
		}
	}
	out = append(out, e.refreshActive(now)...)
	return out
}

func (e *qosEstimator) evaluateLimited(sample qosEstimate, now time.Time) (qosStatus, bool) {
	if sample.SampleTotal == 0 {
		delete(e.limitedSince, sample.DataKind)
		return qosStatus{}, false
	}
	if sample.RateGapRatio < qosRateGapExit || !sample.hasShadowAdvantage(qosShadowAdvantageExit) {
		_, pending := e.limitedSince[sample.DataKind]
		delete(e.limitedSince, sample.DataKind)
		cleared := e.clearActive(sample.DataKind, protocol.LinkStatusReasonLimited)
		if pending || cleared {
			e.recordEvent("limited_clear", sample.DataKind, protocol.LinkStatusReasonLimited)
		}
		return qosStatus{}, false
	}
	if sample.RateGapRatio <= qosRateGapEnter {
		delete(e.limitedSince, sample.DataKind)
		return qosStatus{}, false
	}
	if !sample.hasShadowAdvantage(qosShadowAdvantageEnter) {
		delete(e.limitedSince, sample.DataKind)
		return qosStatus{}, false
	}
	ready, first := e.sustained(e.limitedSince, sample.DataKind, now)
	if !ready {
		if first {
			e.recordEvent("limited_pending", sample.DataKind, protocol.LinkStatusReasonLimited)
		}
		return qosStatus{}, false
	}
	status := qosStatus{
		Kind:         sample.DataKind,
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: sample.DeliveredBps,
	}
	e.setActive(status, now)
	return status, true
}

func (e *qosEstimator) evaluateShadowLimited(sample qosEstimate, now time.Time) (qosStatus, bool) {
	if sample.SampleTotal == 0 || sample.RateGapRatio >= qosRateGapExit || sample.ShadowBps == 0 {
		delete(e.limitedSince, sample.RepairKind)
		return qosStatus{}, false
	}
	if sample.shadowDeficitRatio() <= qosRateGapEnter {
		delete(e.limitedSince, sample.RepairKind)
		return qosStatus{}, false
	}
	ready, first := e.sustained(e.limitedSince, sample.RepairKind, now)
	if !ready {
		if first {
			e.recordEvent("limited_pending", sample.RepairKind, protocol.LinkStatusReasonLimited)
		}
		return qosStatus{}, false
	}
	status := qosStatus{
		Kind:         sample.RepairKind,
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: sample.ShadowBps,
	}
	e.setActive(status, now)
	return status, true
}

func (e *qosEstimator) setActive(status qosStatus, now time.Time) {
	byReason := e.active[status.Kind]
	if byReason == nil {
		byReason = make(map[uint8]qosActiveEvidence)
		e.active[status.Kind] = byReason
	}
	byReason[status.Reason] = qosActiveEvidence{lastEvidence: now, status: status}
}

func (e *qosEstimator) clearActive(kind transport.Kind, reason uint8) bool {
	byReason := e.active[kind]
	if byReason == nil {
		return false
	}
	_, existed := byReason[reason]
	delete(byReason, reason)
	if len(byReason) == 0 {
		delete(e.active, kind)
	}
	return existed
}

func (e *qosEstimator) refreshActive(now time.Time) []qosStatus {
	var out []qosStatus
	for kind, byReason := range e.active {
		for reason, evidence := range byReason {
			if now.Sub(evidence.lastEvidence) > e.cfg.Sustain {
				delete(byReason, reason)
				continue
			}
			if !e.shouldEmit(kind, reason, now) {
				continue
			}
			e.recordEvent(activeEventName(reason), kind, reason)
			out = append(out, evidence.status)
		}
		if len(byReason) == 0 {
			delete(e.active, kind)
		}
	}
	return out
}

func activeEventName(reason uint8) string {
	return "limited_active"
}

func (e *qosEstimator) shouldEmit(kind transport.Kind, reason uint8, now time.Time) bool {
	byReason := e.lastEmit[kind]
	if byReason == nil {
		byReason = make(map[uint8]time.Time)
		e.lastEmit[kind] = byReason
	}
	last := byReason[reason]
	if !last.IsZero() && now.Sub(last) < e.cfg.Refresh {
		return false
	}
	byReason[reason] = now
	return true
}

func (e *qosEstimator) sustained(m map[transport.Kind]time.Time, kind transport.Kind, now time.Time) (ready bool, first bool) {
	since, ok := m[kind]
	if !ok {
		m[kind] = now
		return false, true
	}
	return now.Sub(since) >= e.cfg.Sustain, false
}

func deliveredBps(bytes uint64, duration time.Duration) uint32 {
	if duration <= 0 {
		return 0
	}
	bps := bytes * 8 * uint64(time.Second) / uint64(duration)
	if bps > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(bps)
}

func (e *qosEstimator) resetTransientState() {
	clear(e.limitedSince)
	clear(e.ema)
	clear(e.active)
}

func (e *qosEstimator) hasPendingState() bool {
	return len(e.limitedSince) > 0
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

func (s *qosEMAState) observe(sample qosSample) {
	groupBytes := sample.DataBytes + sample.RecoveredBytes
	s.sampleTotal += sample.DataExpected
	s.sampleCount++
	actualBps := float64(deliveredBps(sample.DataBytes, sample.Duration))
	expectedBps := float64(deliveredBps(groupBytes, sample.Duration))
	repairBps := float64(deliveredBps(sample.RepairBytes, sample.Duration))
	profileData := float64(groupBytes)
	profileFEC := float64(sample.RepairBytes)
	if !s.initialized {
		s.actualBpsEMA = actualBps
		s.expectedBpsEMA = expectedBps
		s.repairBpsEMA = repairBps
		s.profileDataEMA = profileData
		s.profileRepairEMA = profileFEC
		s.initialized = true
		s.trainPID()
		return
	}
	s.actualBpsEMA = emaUpdate(s.actualBpsEMA, actualBps, defaultQoSAlpha)
	s.expectedBpsEMA = emaUpdate(s.expectedBpsEMA, expectedBps, defaultQoSAlpha)
	s.repairBpsEMA = emaUpdate(s.repairBpsEMA, repairBps, defaultQoSAlpha)
	s.profileDataEMA = emaUpdate(s.profileDataEMA, profileData, defaultQoSAlpha)
	s.profileRepairEMA = emaUpdate(s.profileRepairEMA, profileFEC, defaultQoSAlpha)
	s.trainPID()
}

func (s *qosEMAState) estimate(at time.Time) qosEstimate {
	actual := clampUint32Float(s.actualBpsEMA)
	expected := clampUint32Float(s.expectedBpsEMA)
	shadow := clampUint32Float(s.shadowBpsEstimate())
	delivered := actual
	return qosEstimate{
		At:           at,
		DataKind:     s.dataKind,
		RepairKind:   s.repairKind,
		SampleTotal:  s.sampleTotal,
		RateGapRatio: rateGapRatio(s.expectedBpsEMA, s.actualBpsEMA),
		ActualBps:    actual,
		ExpectedBps:  expected,
		ShadowBps:    shadow,
		DeliveredBps: delivered,
	}
}

func (s *qosEMAState) trainPID() {
	referenceBps := s.expectedBpsEMA
	if referenceBps <= 0 || s.shadowBaseBps() <= 0 {
		return
	}
	if rateGapRatio(s.expectedBpsEMA, s.actualBpsEMA) > qosRateGapExit {
		return
	}
	estimate := s.shadowBpsEstimate()
	if s.sampleCount > qosShadowPIDWarmupSamples {
		if shadowDeficitRatio(referenceBps, estimate) > qosShadowPIDGate {
			return
		}
	}
	err := referenceBps - estimate
	s.pidIntegral = qosShadowPIDLeak*s.pidIntegral + err
	derivative := err - s.pidPrevError
	s.pidPrevError = err
	s.pidCorrection = qosShadowPIDLeak*s.pidCorrection +
		qosShadowPIDKp*err +
		qosShadowPIDKi*s.pidIntegral +
		qosShadowPIDKd*derivative
}

func (s *qosEMAState) shadowBaseBps() float64 {
	if s.repairBpsEMA <= 0 || s.profileRepairEMA <= 0 {
		return 0
	}
	return s.repairBpsEMA * s.profileDataEMA / s.profileRepairEMA
}

func (s *qosEMAState) shadowBpsEstimate() float64 {
	base := s.shadowBaseBps()
	if base <= 0 {
		return 0
	}
	shadow := base + s.pidCorrection
	if shadow < 0 {
		return 0
	}
	limit := 4 * s.repairBpsEMA
	if limit > 0 && shadow > limit {
		return limit
	}
	return shadow
}

func (s qosEstimate) hasShadowAdvantage(ratio float64) bool {
	if s.ShadowBps == 0 {
		return false
	}
	if s.ActualBps == 0 {
		return true
	}
	return float64(s.ShadowBps) > float64(s.ActualBps)*ratio
}

func (s qosEstimate) shadowDeficitRatio() float64 {
	reference := s.ExpectedBps
	if reference == 0 {
		reference = s.ActualBps
	}
	return shadowDeficitRatio(float64(reference), float64(s.ShadowBps))
}

func shadowDeficitRatio(actual, shadow float64) float64 {
	if actual <= 0 || shadow >= actual {
		return 0
	}
	return (actual - shadow) / actual
}

func rateGapRatio(expected, actual float64) float64 {
	if expected <= 0 || actual >= expected {
		return 0
	}
	return (expected - actual) / expected
}

func emaUpdate(current, sample, alpha float64) float64 {
	return current + alpha*(sample-current)
}

func clampUint32Float(v float64) uint32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func (e *qosEstimator) recordSampleMetrics(sample qosEstimate) {
	if e == nil || e.cfg.SessionID == 0 {
		return
	}
	leg := kindMetricLabel(sample.DataKind)
	labels := []metrics.Label{
		metrics.LU64("session", e.cfg.SessionID),
		metrics.LU8("lane", e.cfg.LaneID),
		metrics.LStr("leg", leg),
	}
	metrics.SetGauge(metrics.QoSDeliveredBps, float64(sample.DeliveredBps), labels...)
}

func (e *qosEstimator) recordEvent(event string, kind transport.Kind, reason uint8) {
	if e == nil || e.cfg.SessionID == 0 {
		return
	}
	metrics.IncCounter(metrics.QoSEventsTotal,
		metrics.LStr("event", event),
		metrics.LU64("session", e.cfg.SessionID),
		metrics.LU8("lane", e.cfg.LaneID),
		metrics.LStr("leg", kindMetricLabel(kind)),
		metrics.LU8("reason", reason),
	)
	switch event {
	case "limited_active", "limited_clear":
		eventlog.Printf("qos_state", "event=%s session=%d lane=%d leg=%s reason=%d",
			event, e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(kind), reason)
	}
}
