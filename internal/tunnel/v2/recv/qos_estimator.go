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
	defaultQoSLagSlack    = 300 * time.Millisecond
	defaultQoSRefresh     = time.Second
	defaultQoSAlpha       = 0.10

	qosLossEnter = 0.05
	qosLossExit  = 0.01
)

type qosConfig struct {
	Sustain     time.Duration
	SampleFloor uint64
	LagSlack    time.Duration
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
	Lag            time.Duration
}

type qosEstimator struct {
	cfg  qosConfig
	emit func(qosStatus)

	limitedSince map[transport.Kind]time.Time
	backlogSince map[transport.Kind]time.Time
	lagBaseline  map[transport.Kind]time.Duration
	lastEmit     map[transport.Kind]map[uint8]time.Time
	ema          map[transport.Kind]*qosEMAState
	active       map[transport.Kind]map[uint8]qosActiveEvidence
}

type qosEstimate struct {
	At           time.Time
	DataKind     transport.Kind
	RepairKind   transport.Kind
	SampleTotal  uint64
	Loss         float64
	DeliveredBps uint32
	Lag          time.Duration
}

type qosEMAState struct {
	repairKind         transport.Kind
	initialized        bool
	sampleTotal        uint64
	expectedEMA        float64
	arrivedEMA         float64
	deliveredBpsEMA    float64
	lagMillisecondsEMA float64
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
	if cfg.LagSlack <= 0 {
		cfg.LagSlack = defaultQoSLagSlack
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = defaultQoSRefresh
	}
	return &qosEstimator{
		cfg:          cfg,
		emit:         emit,
		limitedSince: make(map[transport.Kind]time.Time),
		backlogSince: make(map[transport.Kind]time.Time),
		lagBaseline:  make(map[transport.Kind]time.Duration),
		lastEmit:     make(map[transport.Kind]map[uint8]time.Time),
		ema:          make(map[transport.Kind]*qosEMAState),
		active:       make(map[transport.Kind]map[uint8]qosActiveEvidence),
	}
}

func (e *qosEstimator) Observe(sample qosSample) {
	if !qosKnownKind(sample.DataKind) {
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
	state := e.ema[sample.DataKind]
	if state == nil || state.repairKind != sample.RepairKind {
		state = &qosEMAState{repairKind: sample.RepairKind}
		e.ema[sample.DataKind] = state
	}
	state.observe(sample)
	if state.sampleTotal < e.cfg.SampleFloor {
		e.recordEvent("below_floor", sample.DataKind, 0)
		return qosEstimate{}, false
	}
	estimate := state.estimate(now, sample.DataKind)
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
	if status, ok := e.evaluateBacklogged(sample, now); ok {
		if e.shouldEmit(status.Kind, status.Reason, now) {
			e.recordEvent("backlogged_active", status.Kind, status.Reason)
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
	if sample.Loss < qosLossExit {
		delete(e.limitedSince, sample.DataKind)
		e.clearActive(sample.DataKind, protocol.LinkStatusReasonLimited)
		return qosStatus{}, false
	}
	if sample.Loss <= qosLossEnter {
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

func (e *qosEstimator) evaluateBacklogged(sample qosEstimate, now time.Time) (qosStatus, bool) {
	if sample.Lag <= 0 || !qosKnownKind(sample.RepairKind) || sample.RepairKind == sample.DataKind {
		delete(e.backlogSince, sample.DataKind)
		e.clearActive(sample.DataKind, protocol.LinkStatusReasonBacklogged)
		return qosStatus{}, false
	}
	base, hasBase := e.lagBaseline[sample.DataKind]
	if !hasBase || sample.Lag < base {
		e.lagBaseline[sample.DataKind] = sample.Lag
		base = sample.Lag
	}
	if sample.Lag <= base+e.cfg.LagSlack {
		delete(e.backlogSince, sample.DataKind)
		e.clearActive(sample.DataKind, protocol.LinkStatusReasonBacklogged)
		return qosStatus{}, false
	}
	ready, first := e.sustained(e.backlogSince, sample.DataKind, now)
	if !ready {
		if first {
			e.recordEvent("backlogged_pending", sample.DataKind, protocol.LinkStatusReasonBacklogged)
		}
		return qosStatus{}, false
	}
	status := qosStatus{
		Kind:         sample.DataKind,
		Reason:       protocol.LinkStatusReasonBacklogged,
		DeliveredBps: sample.DeliveredBps,
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

func (e *qosEstimator) clearActive(kind transport.Kind, reason uint8) {
	byReason := e.active[kind]
	if byReason == nil {
		return
	}
	delete(byReason, reason)
	if len(byReason) == 0 {
		delete(e.active, kind)
	}
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
	if reason == protocol.LinkStatusReasonBacklogged {
		return "backlogged_active"
	}
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
	clear(e.backlogSince)
	clear(e.ema)
	clear(e.active)
}

func (e *qosEstimator) hasPendingState() bool {
	return len(e.limitedSince) > 0 || len(e.backlogSince) > 0
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
	s.sampleTotal += sample.DataExpected
	expected := float64(sample.DataExpected)
	arrived := float64(sample.DataArrived)
	bps := float64(deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration))
	lagMS := float64(sample.Lag.Milliseconds())
	if !s.initialized {
		s.expectedEMA = expected
		s.arrivedEMA = arrived
		s.deliveredBpsEMA = bps
		s.lagMillisecondsEMA = lagMS
		s.initialized = true
		return
	}
	s.expectedEMA = emaUpdate(s.expectedEMA, expected, defaultQoSAlpha)
	s.arrivedEMA = emaUpdate(s.arrivedEMA, arrived, defaultQoSAlpha)
	s.deliveredBpsEMA = emaUpdate(s.deliveredBpsEMA, bps, defaultQoSAlpha)
	s.lagMillisecondsEMA = emaUpdate(s.lagMillisecondsEMA, lagMS, defaultQoSAlpha)
}

func (s *qosEMAState) estimate(at time.Time, kind transport.Kind) qosEstimate {
	loss := 0.0
	if s.expectedEMA > 0 && s.arrivedEMA < s.expectedEMA {
		loss = (s.expectedEMA - s.arrivedEMA) / s.expectedEMA
	}
	delivered := s.deliveredBpsEMA
	if delivered < 0 {
		delivered = 0
	}
	if delivered > math.MaxUint32 {
		delivered = math.MaxUint32
	}
	lagMS := s.lagMillisecondsEMA
	if lagMS < 0 {
		lagMS = 0
	}
	return qosEstimate{
		At:           at,
		DataKind:     kind,
		RepairKind:   s.repairKind,
		SampleTotal:  s.sampleTotal,
		Loss:         loss,
		DeliveredBps: uint32(delivered),
		Lag:          time.Duration(lagMS) * time.Millisecond,
	}
}

func emaUpdate(current, sample, alpha float64) float64 {
	return current + alpha*(sample-current)
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
	metrics.SetGauge(metrics.QoSLossRatio, sample.Loss, labels...)
	metrics.SetGauge(metrics.QoSLagMs, float64(sample.Lag.Milliseconds()), labels...)
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
	case "limited_pending", "limited_active", "backlogged_pending", "backlogged_active":
		eventlog.Printf("qos_state", "event=%s session=%d lane=%d leg=%s reason=%d",
			event, e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(kind), reason)
	}
}
