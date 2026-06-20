package recv

import (
	"math"
	"time"

	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultQoSSustain     = 3 * time.Second
	defaultQoSSampleFloor = 4
	defaultQoSAlpha       = 0.10

	qosRateGapEnter       = 0.10
	qosRateGapExit        = 0.03
	qosSevereRateGapEnter = 0.50
	qosSevereRateSustain  = time.Second

	qosFECHealthEnter   = 0.70
	qosFECHealthExit    = 0.90
	qosFECHealthSustain = time.Second

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
	SessionID   uint64
	LaneID      uint8
}

type qosStatus struct {
	UDPLimited      bool
	TCPLimited      bool
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
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
	RepairAt       time.Time
	RepairBytes    uint64
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
	cfg  qosConfig
	emit func(qosStatus)

	limitedSince map[qosEvidenceKey]time.Time
	ema          map[qosDirectionKey]*qosEMAState
	health       map[qosDirectionKey]*qosHealthState
	active       map[transport.Kind]map[qosEvidence]qosActiveEvidence
}

type qosDirectionKey struct {
	dataKind   transport.Kind
	repairKind transport.Kind
}

type qosEvidence uint8

const (
	qosEvidenceRate qosEvidence = iota + 1
	qosEvidenceShadow
	qosEvidenceHealth
	qosEvidenceShadowClear
)

type qosEvidenceKey struct {
	kind     transport.Kind
	evidence qosEvidence
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
	dataKind          transport.Kind
	repairKind        transport.Kind
	initialized       bool
	sampleCount       uint64
	sampleTotal       uint64
	actualBpsEMA      float64
	expectedBpsEMA    float64
	repairBpsEMA      float64
	repairInitialized bool
	profileDataEMA    float64
	profileRepairEMA  float64
	lastRepairAt      time.Time
	pidCorrection     float64
	pidIntegral       float64
	pidPrevError      float64
}

type qosHealthState struct {
	initialized  bool
	sampleTotal  uint64
	healthEMA    float64
	deliveredBps uint32
}

type qosActiveEvidence struct {
	deliveredBps uint32
}

func newQoSEstimator(cfg qosConfig, emit func(qosStatus)) *qosEstimator {
	if cfg.Sustain <= 0 {
		cfg.Sustain = defaultQoSSustain
	}
	if cfg.SampleFloor == 0 {
		cfg.SampleFloor = defaultQoSSampleFloor
	}
	return &qosEstimator{
		cfg:          cfg,
		emit:         emit,
		limitedSince: make(map[qosEvidenceKey]time.Time),
		ema:          make(map[qosDirectionKey]*qosEMAState),
		health:       make(map[qosDirectionKey]*qosHealthState),
		active:       make(map[transport.Kind]map[qosEvidence]qosActiveEvidence),
	}
}

func (e *qosEstimator) Observe(sample qosSample) {
	if !qosKnownKind(sample.DataKind) {
		return
	}
	if !qosKnownKind(sample.RepairKind) || sample.DataKind == sample.RepairKind {
		return
	}
	var out []qosStatus
	if estimate, ok := e.updateEstimate(sample); ok {
		out = append(out, e.evaluate(estimate)...)
	}
	if status, ok := e.ObserveHealth(qosHealthSample{
		At:           sample.At,
		DataKind:     sample.DataKind,
		RepairKind:   sample.RepairKind,
		DataArrived:  sample.DataExpected,
		DataExpected: sample.DataExpected,
		DeliveredBps: deliveredBps(sample.DataBytes, sample.Duration),
	}); ok {
		out = append(out, status)
	}
	e.emitLast(out)
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
		e.recordEvent("below_floor", sample.DataKind)
		return qosEstimate{}, false
	}
	estimate := state.estimate(now)
	e.recordSampleMetrics(estimate)
	return estimate, true
}

func (e *qosEstimator) evaluate(sample qosEstimate) []qosStatus {
	now := sample.At
	if now.IsZero() {
		now = time.Now()
	}
	changed := e.evaluateLimited(sample, now)
	changed = e.evaluateShadowLimited(sample, now) || changed
	if changed {
		return []qosStatus{e.snapshot(sample)}
	}
	return nil
}

func (e *qosEstimator) evaluateLimited(sample qosEstimate, now time.Time) bool {
	if sample.SampleTotal == 0 {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceRate})
		return false
	}
	if e.kindActive(sample.RepairKind) {
		key := qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceRate}
		delete(e.limitedSince, key)
		cleared := e.clearActive(sample.DataKind, qosEvidenceRate)
		if cleared {
			e.recordEvent("limited_clear", sample.DataKind)
		}
		return cleared
	}
	if sample.RateGapRatio < qosRateGapExit || !sample.hasShadowAdvantage(qosShadowAdvantageExit) {
		key := qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceRate}
		delete(e.limitedSince, key)
		cleared := e.clearActive(sample.DataKind, qosEvidenceRate)
		if cleared {
			e.recordEvent("limited_clear", sample.DataKind)
		}
		return cleared
	}
	if sample.RateGapRatio <= qosRateGapEnter {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceRate})
		return false
	}
	if !sample.hasShadowAdvantage(qosShadowAdvantageEnter) {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceRate})
		return false
	}
	if e.isActive(sample.DataKind, qosEvidenceRate) {
		e.setActive(sample.DataKind, qosEvidenceRate, sample.DeliveredBps)
		return false
	}
	ready, first := e.sustainedFor(sample.DataKind, qosEvidenceRate, now, e.rateSustain(sample.RateGapRatio))
	if !ready {
		if first {
			e.recordEvent("limited_pending", sample.DataKind)
		}
		return false
	}
	e.setActive(sample.DataKind, qosEvidenceRate, sample.DeliveredBps)
	e.recordEvent("limited_active", sample.DataKind)
	return true
}

func (e *qosEstimator) evaluateShadowLimited(sample qosEstimate, now time.Time) bool {
	if sample.SampleTotal == 0 || sample.ShadowBps == 0 {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadow})
		delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadowClear})
		return false
	}
	deficit := sample.shadowDeficitRatio()
	if deficit < qosRateGapExit {
		if !e.kindActive(sample.RepairKind) {
			delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadow})
			delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadowClear})
			return false
		}
		ready, _ := e.sustainedFor(sample.RepairKind, qosEvidenceShadowClear, now, e.cfg.Sustain)
		if !ready {
			delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadow})
			return false
		}
		pending := e.clearKindPending(sample.RepairKind)
		cleared := e.clearKindActive(sample.RepairKind)
		if cleared {
			e.recordEvent("limited_clear", sample.RepairKind)
		} else if !pending {
			delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadowClear})
		}
		return cleared
	}
	delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadowClear})
	if sample.RateGapRatio >= qosRateGapExit {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadow})
		return false
	}
	if deficit <= qosRateGapEnter {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.RepairKind, evidence: qosEvidenceShadow})
		return false
	}
	if e.isActive(sample.RepairKind, qosEvidenceShadow) {
		e.setActive(sample.RepairKind, qosEvidenceShadow, sample.ShadowBps)
		return false
	}
	ready, first := e.sustained(sample.RepairKind, qosEvidenceShadow, now)
	if !ready {
		if first {
			e.recordEvent("limited_pending", sample.RepairKind)
		}
		return false
	}
	e.setActive(sample.RepairKind, qosEvidenceShadow, sample.ShadowBps)
	e.recordEvent("limited_active", sample.RepairKind)
	return true
}

func (e *qosEstimator) ObserveHealth(sample qosHealthSample) (qosStatus, bool) {
	if !qosKnownKind(sample.DataKind) {
		return qosStatus{}, false
	}
	if !qosKnownKind(sample.RepairKind) || sample.DataKind == sample.RepairKind || sample.DataExpected == 0 {
		return qosStatus{}, false
	}
	now := sample.At
	if now.IsZero() {
		now = time.Now()
	}
	key := qosDirectionKey{dataKind: sample.DataKind, repairKind: sample.RepairKind}
	state := e.health[key]
	if state == nil {
		state = &qosHealthState{}
		e.health[key] = state
	}
	score := fecHealthScore(sample.DataArrived, sample.DataExpected)
	state.observe(score, sample.DataExpected, sample.DeliveredBps)
	if state.sampleTotal < e.cfg.SampleFloor {
		return qosStatus{}, false
	}
	if state.healthEMA >= qosFECHealthExit {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceHealth})
		cleared := e.clearActive(sample.DataKind, qosEvidenceHealth)
		if cleared {
			e.recordEvent("limited_clear", sample.DataKind)
			return e.snapshotForKind(sample.DataKind), true
		}
		return qosStatus{}, false
	}
	if state.healthEMA >= qosFECHealthEnter {
		delete(e.limitedSince, qosEvidenceKey{kind: sample.DataKind, evidence: qosEvidenceHealth})
		return qosStatus{}, false
	}
	if e.isActive(sample.DataKind, qosEvidenceHealth) {
		e.setActive(sample.DataKind, qosEvidenceHealth, state.deliveredBps)
		return qosStatus{}, false
	}
	ready, first := e.sustained(sample.DataKind, qosEvidenceHealth, now)
	if !ready {
		if first {
			e.recordEvent("limited_pending", sample.DataKind)
		}
		return qosStatus{}, false
	}
	e.setActive(sample.DataKind, qosEvidenceHealth, state.deliveredBps)
	e.recordEvent("limited_active", sample.DataKind)
	return e.snapshotForKind(sample.DataKind), true
}

func fecHealthScore(arrived, expected uint64) float64 {
	if expected == 0 {
		return 1
	}
	if arrived+1 >= expected {
		return 1
	}
	return float64(arrived) / float64(expected)
}

func (s *qosHealthState) observe(score float64, expected uint64, deliveredBps uint32) {
	s.sampleTotal += expected
	s.deliveredBps = deliveredBps
	if !s.initialized {
		s.healthEMA = score
		s.initialized = true
		return
	}
	s.healthEMA = emaUpdate(s.healthEMA, score, defaultQoSAlpha)
}

func (e *qosEstimator) setActive(kind transport.Kind, evidence qosEvidence, deliveredBps uint32) {
	byEvidence := e.active[kind]
	if byEvidence == nil {
		byEvidence = make(map[qosEvidence]qosActiveEvidence)
		e.active[kind] = byEvidence
	}
	byEvidence[evidence] = qosActiveEvidence{deliveredBps: deliveredBps}
}

func (e *qosEstimator) isActive(kind transport.Kind, evidence qosEvidence) bool {
	byEvidence := e.active[kind]
	if byEvidence == nil {
		return false
	}
	_, ok := byEvidence[evidence]
	return ok
}

func (e *qosEstimator) kindActive(kind transport.Kind) bool {
	byEvidence := e.active[kind]
	return len(byEvidence) > 0
}

func (e *qosEstimator) clearActive(kind transport.Kind, evidence qosEvidence) bool {
	byEvidence := e.active[kind]
	if byEvidence == nil {
		return false
	}
	_, existed := byEvidence[evidence]
	delete(byEvidence, evidence)
	if len(byEvidence) == 0 {
		delete(e.active, kind)
	}
	return existed
}

func (e *qosEstimator) clearKindActive(kind transport.Kind) bool {
	byEvidence := e.active[kind]
	if len(byEvidence) == 0 {
		return false
	}
	delete(e.active, kind)
	return true
}

func (e *qosEstimator) clearKindPending(kind transport.Kind) bool {
	cleared := false
	for key := range e.limitedSince {
		if key.kind != kind {
			continue
		}
		delete(e.limitedSince, key)
		cleared = true
	}
	return cleared
}

func (e *qosEstimator) snapshot(sample qosEstimate) qosStatus {
	status := qosStatus{}
	switch sample.DataKind {
	case transport.KindUDP:
		status.UDPDeliveredBps = sample.ActualBps
	case transport.KindTCP:
		status.TCPDeliveredBps = sample.ActualBps
	}
	switch sample.RepairKind {
	case transport.KindUDP:
		status.UDPDeliveredBps = sample.ShadowBps
	case transport.KindTCP:
		status.TCPDeliveredBps = sample.ShadowBps
	}
	if bps, ok := e.activeDeliveredBps(transport.KindUDP); ok {
		status.UDPLimited = true
		status.UDPDeliveredBps = bps
	}
	if bps, ok := e.activeDeliveredBps(transport.KindTCP); ok {
		status.TCPLimited = true
		status.TCPDeliveredBps = bps
	}
	return status
}

func (e *qosEstimator) snapshotForKind(kind transport.Kind) qosStatus {
	status := qosStatus{}
	if bps, ok := e.activeDeliveredBps(transport.KindUDP); ok {
		status.UDPLimited = true
		status.UDPDeliveredBps = bps
	}
	if bps, ok := e.activeDeliveredBps(transport.KindTCP); ok {
		status.TCPLimited = true
		status.TCPDeliveredBps = bps
	}
	if !status.UDPLimited && kind == transport.KindUDP {
		status.UDPDeliveredBps = e.lastDeliveredBps(kind)
	}
	if !status.TCPLimited && kind == transport.KindTCP {
		status.TCPDeliveredBps = e.lastDeliveredBps(kind)
	}
	return status
}

func (e *qosEstimator) activeDeliveredBps(kind transport.Kind) (uint32, bool) {
	byEvidence := e.active[kind]
	if len(byEvidence) == 0 {
		return 0, false
	}
	var out uint32
	first := true
	for _, evidence := range byEvidence {
		bps := evidence.deliveredBps
		if first || bps < out {
			out = bps
			first = false
		}
	}
	return out, true
}

func (e *qosEstimator) lastDeliveredBps(kind transport.Kind) uint32 {
	var out uint32
	for key, state := range e.ema {
		if key.dataKind == kind {
			bps := clampUint32Float(state.actualBpsEMA)
			if bps != 0 && (out == 0 || bps < out) {
				out = bps
			}
		}
	}
	return out
}

func (e *qosEstimator) sustained(kind transport.Kind, evidence qosEvidence, now time.Time) (ready bool, first bool) {
	return e.sustainedFor(kind, evidence, now, e.sustainFor(evidence))
}

func (e *qosEstimator) sustainedFor(kind transport.Kind, evidence qosEvidence, now time.Time, sustain time.Duration) (ready bool, first bool) {
	key := qosEvidenceKey{kind: kind, evidence: evidence}
	since, ok := e.limitedSince[key]
	if !ok {
		e.limitedSince[key] = now
		return false, true
	}
	return now.Sub(since) >= sustain, false
}

func (e *qosEstimator) sustainFor(evidence qosEvidence) time.Duration {
	if evidence == qosEvidenceHealth && qosFECHealthSustain < e.cfg.Sustain {
		return qosFECHealthSustain
	}
	return e.cfg.Sustain
}

func (e *qosEstimator) rateSustain(rateGapRatio float64) time.Duration {
	if rateGapRatio >= qosSevereRateGapEnter && qosSevereRateSustain < e.cfg.Sustain {
		return qosSevereRateSustain
	}
	return e.cfg.Sustain
}

func (e *qosEstimator) emitLast(statuses []qosStatus) {
	if e.emit == nil || len(statuses) == 0 {
		return
	}
	e.emit(statuses[len(statuses)-1])
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
	repairBps, hasRepairBps := s.repairIntervalBps(sample)
	profileData := float64(groupBytes)
	profileFEC := float64(sample.RepairBytes)
	if !s.initialized {
		s.actualBpsEMA = actualBps
		s.expectedBpsEMA = expectedBps
		if hasRepairBps {
			s.repairBpsEMA = repairBps
			s.repairInitialized = true
		}
		s.profileDataEMA = profileData
		s.profileRepairEMA = profileFEC
		s.initialized = true
		s.trainPID()
		return
	}
	s.actualBpsEMA = emaUpdate(s.actualBpsEMA, actualBps, defaultQoSAlpha)
	s.expectedBpsEMA = emaUpdate(s.expectedBpsEMA, expectedBps, defaultQoSAlpha)
	if hasRepairBps {
		if s.repairInitialized {
			s.repairBpsEMA = emaUpdate(s.repairBpsEMA, repairBps, defaultQoSAlpha)
		} else {
			s.repairBpsEMA = repairBps
			s.repairInitialized = true
		}
	}
	s.profileDataEMA = emaUpdate(s.profileDataEMA, profileData, defaultQoSAlpha)
	s.profileRepairEMA = emaUpdate(s.profileRepairEMA, profileFEC, defaultQoSAlpha)
	s.trainPID()
}

func (s *qosEMAState) estimate(at time.Time) qosEstimate {
	actual := clampUint32Float(s.actualBpsEMA)
	shadowEstimate := s.shadowBpsEstimate()
	reference := s.expectedBpsEMA
	if shadowEstimate > reference {
		reference = shadowEstimate
	}
	expected := clampUint32Float(reference)
	shadow := clampUint32Float(shadowEstimate)
	delivered := actual
	return qosEstimate{
		At:           at,
		DataKind:     s.dataKind,
		RepairKind:   s.repairKind,
		SampleTotal:  s.sampleTotal,
		RateGapRatio: rateGapRatio(reference, s.actualBpsEMA),
		ActualBps:    actual,
		ExpectedBps:  expected,
		ShadowBps:    shadow,
		DeliveredBps: delivered,
	}
}

func (s *qosEMAState) repairIntervalBps(sample qosSample) (float64, bool) {
	if sample.RepairBytes == 0 {
		return 0, false
	}
	repairAt := sample.RepairAt
	if repairAt.IsZero() {
		repairAt = sample.At
	}
	if repairAt.IsZero() {
		return 0, false
	}
	if s.lastRepairAt.IsZero() {
		if sample.Duration <= 0 {
			s.lastRepairAt = repairAt
			return 0, false
		}
		s.lastRepairAt = repairAt
		return float64(deliveredBps(sample.RepairBytes, sample.Duration)), true
	}
	if !repairAt.After(s.lastRepairAt) {
		s.lastRepairAt = repairAt
		return 0, false
	}
	duration := repairAt.Sub(s.lastRepairAt)
	s.lastRepairAt = repairAt
	return float64(deliveredBps(sample.RepairBytes, duration)), true
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
	if rateGapRatio(estimate, s.actualBpsEMA) > qosShadowPIDGate {
		return
	}
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

func (e *qosEstimator) recordEvent(event string, kind transport.Kind) {
	if e == nil || e.cfg.SessionID == 0 {
		return
	}
	metrics.IncCounter(metrics.QoSEventsTotal,
		metrics.LStr("event", event),
		metrics.LU64("session", e.cfg.SessionID),
		metrics.LU8("lane", e.cfg.LaneID),
		metrics.LStr("leg", kindMetricLabel(kind)),
	)
	switch event {
	case "limited_active", "limited_clear":
		eventlog.Printf("qos_state", "event=%s session=%d lane=%d leg=%s",
			event, e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(kind))
	}
}
