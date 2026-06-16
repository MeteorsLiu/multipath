package recv

import (
	"math"
	"sort"
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

	qosShadowQuantile       = 0.25
	qosShadowAdvantageEnter = 1.10
	qosShadowAdvantageExit  = 1.05
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
	ema          map[transport.Kind]*qosEMAState
	active       map[transport.Kind]map[uint8]qosActiveEvidence
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
	repairKind     transport.Kind
	initialized    bool
	sampleTotal    uint64
	actualBpsEMA   float64
	expectedBpsEMA float64
	repairBpsEMA   float64
	betaQ25        qosQuantile
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
		ema:          make(map[transport.Kind]*qosEMAState),
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
	s.sampleTotal += sample.DataExpected
	actualBps := float64(deliveredBps(sample.DataBytes, sample.Duration))
	expectedBps := float64(deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration))
	repairBps := float64(deliveredBps(sample.RepairBytes, sample.Duration))
	if factor, ok := shadowBetaFactor(sample); ok {
		s.betaQ25.observe(factor)
	}
	if !s.initialized {
		s.actualBpsEMA = actualBps
		s.expectedBpsEMA = expectedBps
		s.repairBpsEMA = repairBps
		s.initialized = true
		return
	}
	s.actualBpsEMA = emaUpdate(s.actualBpsEMA, actualBps, defaultQoSAlpha)
	s.expectedBpsEMA = emaUpdate(s.expectedBpsEMA, expectedBps, defaultQoSAlpha)
	s.repairBpsEMA = emaUpdate(s.repairBpsEMA, repairBps, defaultQoSAlpha)
}

func (s *qosEMAState) estimate(at time.Time, kind transport.Kind) qosEstimate {
	actual := clampUint32Float(s.actualBpsEMA)
	expected := clampUint32Float(s.expectedBpsEMA)
	shadow := clampUint32Float(s.repairBpsEMA * s.betaQ25.value())
	delivered := actual
	rateGapRatio := 0.0
	if s.expectedBpsEMA > 0 && s.actualBpsEMA < s.expectedBpsEMA {
		rateGapRatio = (s.expectedBpsEMA - s.actualBpsEMA) / s.expectedBpsEMA
	}
	return qosEstimate{
		At:           at,
		DataKind:     kind,
		RepairKind:   s.repairKind,
		SampleTotal:  s.sampleTotal,
		RateGapRatio: rateGapRatio,
		ActualBps:    actual,
		ExpectedBps:  expected,
		ShadowBps:    shadow,
		DeliveredBps: delivered,
	}
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

func shadowBetaFactor(sample qosSample) (float64, bool) {
	groupBytes := sample.DataBytes + sample.RecoveredBytes
	if groupBytes == 0 || sample.RepairBytes == 0 {
		return 0, false
	}
	factor := float64(groupBytes) / float64(sample.RepairBytes)
	if factor < 1 {
		factor = 1
	}
	maxFactor := float64(sample.DataExpected)
	if maxFactor < 1 {
		maxFactor = 1
	}
	if factor > maxFactor {
		factor = maxFactor
	}
	return factor, true
}

type qosQuantile struct {
	initialized bool
	initial     []float64
	q           [5]float64
	n           [5]int
	np          [5]float64
	dn          [5]float64
}

func (q *qosQuantile) observe(v float64) {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if !q.initialized {
		q.initial = append(q.initial, v)
		if len(q.initial) < 5 {
			return
		}
		sort.Float64s(q.initial)
		copy(q.q[:], q.initial)
		q.n = [5]int{1, 2, 3, 4, 5}
		q.np = [5]float64{1, 1 + 2*qosShadowQuantile, 1 + 4*qosShadowQuantile, 3 + 2*qosShadowQuantile, 5}
		q.dn = [5]float64{0, qosShadowQuantile / 2, qosShadowQuantile, (1 + qosShadowQuantile) / 2, 1}
		q.initialized = true
		return
	}

	k := 0
	switch {
	case v < q.q[0]:
		q.q[0] = v
	case v < q.q[1]:
		k = 0
	case v < q.q[2]:
		k = 1
	case v < q.q[3]:
		k = 2
	case v <= q.q[4]:
		k = 3
	default:
		q.q[4] = v
		k = 3
	}
	for i := k + 1; i < 5; i++ {
		q.n[i]++
	}
	for i := range q.np {
		q.np[i] += q.dn[i]
	}
	for i := 1; i < 4; i++ {
		d := q.np[i] - float64(q.n[i])
		if (d >= 1 && q.n[i+1]-q.n[i] > 1) || (d <= -1 && q.n[i-1]-q.n[i] < -1) {
			step := 1
			if d < 0 {
				step = -1
			}
			next := q.parabolic(i, step)
			if next <= q.q[i-1] || next >= q.q[i+1] {
				next = q.linear(i, step)
			}
			q.q[i] = next
			q.n[i] += step
		}
	}
}

func (q *qosQuantile) value() float64 {
	if !q.initialized {
		if len(q.initial) == 0 {
			return 0
		}
		values := append([]float64(nil), q.initial...)
		sort.Float64s(values)
		idx := int(math.Floor(qosShadowQuantile * float64(len(values)-1)))
		return values[idx]
	}
	return q.q[2]
}

func (q *qosQuantile) parabolic(i, step int) float64 {
	n := q.n
	v := q.q
	return v[i] + float64(step)/float64(n[i+1]-n[i-1])*
		((float64(n[i]-n[i-1]+step)*(v[i+1]-v[i])/float64(n[i+1]-n[i]))+
			(float64(n[i+1]-n[i]-step)*(v[i]-v[i-1])/float64(n[i]-n[i-1])))
}

func (q *qosQuantile) linear(i, step int) float64 {
	j := i + step
	return q.q[i] + float64(step)*(q.q[j]-q.q[i])/float64(q.n[j]-q.n[i])
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
