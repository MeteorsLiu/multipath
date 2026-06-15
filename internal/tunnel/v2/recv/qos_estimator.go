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
	windows      map[transport.Kind]*qosSampleWindow
	active       map[transport.Kind]map[uint8]qosActiveEvidence
}

type qosSampleWindow struct {
	repairKind transport.Kind
	samples    []qosSample
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
		windows:      make(map[transport.Kind]*qosSampleWindow),
		active:       make(map[transport.Kind]map[uint8]qosActiveEvidence),
	}
}

func (e *qosEstimator) Observe(sample qosSample) {
	if !qosKnownKind(sample.DataKind) {
		return
	}
	sample, ok := e.windowSample(sample)
	if !ok {
		return
	}
	for _, status := range e.evaluate(sample) {
		if e.emit != nil {
			e.emit(status)
		}
	}
}

func (e *qosEstimator) windowSample(sample qosSample) (qosSample, bool) {
	now := sample.At
	if now.IsZero() {
		now = time.Now()
		sample.At = now
	}
	window := e.windows[sample.DataKind]
	if window == nil || window.repairKind != sample.RepairKind {
		window = &qosSampleWindow{repairKind: sample.RepairKind}
		e.windows[sample.DataKind] = window
	}
	window.samples = append(window.samples, sample)
	window.prune(now.Add(-e.cfg.Sustain))
	aggregate, ok := window.aggregate()
	if !ok || aggregate.DataExpected < e.cfg.SampleFloor {
		e.recordEvent("below_floor", sample.DataKind, 0)
		return qosSample{}, false
	}
	e.recordSampleMetrics(aggregate)
	return aggregate, true
}

func (e *qosEstimator) evaluate(sample qosSample) []qosStatus {
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

func (e *qosEstimator) evaluateLimited(sample qosSample, now time.Time) (qosStatus, bool) {
	expected := sample.DataExpected
	if expected == 0 {
		delete(e.limitedSince, sample.DataKind)
		return qosStatus{}, false
	}
	got := sample.DataArrived
	loss := float64(expected-got) / float64(expected)
	if got > expected {
		loss = 0
	}
	if loss < qosLossExit {
		delete(e.limitedSince, sample.DataKind)
		e.clearActive(sample.DataKind, protocol.LinkStatusReasonLimited)
		return qosStatus{}, false
	}
	if loss <= qosLossEnter {
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
		DeliveredBps: deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration),
	}
	e.setActive(status, now)
	return status, true
}

func (e *qosEstimator) evaluateBacklogged(sample qosSample, now time.Time) (qosStatus, bool) {
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
		DeliveredBps: deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration),
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
	clear(e.windows)
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

func mergeQoSSamples(a, b qosSample) qosSample {
	start := qosSampleStart(a)
	if bStart := qosSampleStart(b); start.IsZero() || (!bStart.IsZero() && bStart.Before(start)) {
		start = bStart
	}
	end := a.At
	if b.At.After(end) {
		end = b.At
	}
	out := qosSample{
		At:             end,
		DataKind:       a.DataKind,
		DataArrived:    a.DataArrived + b.DataArrived,
		DataExpected:   a.DataExpected + b.DataExpected,
		DataBytes:      a.DataBytes + b.DataBytes,
		RecoveredBytes: a.RecoveredBytes + b.RecoveredBytes,
		RepairKind:     a.RepairKind,
		RepairBytes:    a.RepairBytes + b.RepairBytes,
		Lag:            a.Lag,
	}
	if b.Lag > out.Lag {
		out.Lag = b.Lag
	}
	out.Duration = sampleDuration(start, end)
	return out
}

func qosSampleStart(sample qosSample) time.Time {
	if sample.At.IsZero() || sample.Duration <= 0 {
		return sample.At
	}
	return sample.At.Add(-sample.Duration)
}

func (w *qosSampleWindow) prune(cutoff time.Time) {
	if w == nil || cutoff.IsZero() {
		return
	}
	keep := 0
	for keep < len(w.samples) {
		sample := w.samples[keep]
		if sample.At.IsZero() || !sample.At.Before(cutoff) {
			break
		}
		keep++
	}
	if keep == 0 {
		return
	}
	copy(w.samples, w.samples[keep:])
	clear(w.samples[len(w.samples)-keep:])
	w.samples = w.samples[:len(w.samples)-keep]
}

func (w *qosSampleWindow) aggregate() (qosSample, bool) {
	if w == nil || len(w.samples) == 0 {
		return qosSample{}, false
	}
	out := w.samples[0]
	for i := 1; i < len(w.samples); i++ {
		out = mergeQoSSamples(out, w.samples[i])
	}
	return out, true
}

func (e *qosEstimator) recordSampleMetrics(sample qosSample) {
	if e == nil || e.cfg.SessionID == 0 {
		return
	}
	leg := kindMetricLabel(sample.DataKind)
	loss := 0.0
	if sample.DataExpected > 0 && sample.DataArrived < sample.DataExpected {
		loss = float64(sample.DataExpected-sample.DataArrived) / float64(sample.DataExpected)
	}
	labels := []metrics.Label{
		metrics.LU64("session", e.cfg.SessionID),
		metrics.LU8("lane", e.cfg.LaneID),
		metrics.LStr("leg", leg),
	}
	metrics.SetGauge(metrics.QoSLossRatio, loss, labels...)
	metrics.SetGauge(metrics.QoSLagMs, float64(sample.Lag.Milliseconds()), labels...)
	metrics.SetGauge(metrics.QoSDeliveredBps, float64(deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration)), labels...)
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
