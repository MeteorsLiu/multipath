package recv

import (
	"math"
	"time"

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
	pending      map[transport.Kind]qosSample
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
		pending:      make(map[transport.Kind]qosSample),
	}
}

func (e *qosEstimator) Observe(sample qosSample) {
	if !qosKnownKind(sample.DataKind) {
		return
	}
	sample, ok := e.floorSample(sample)
	if !ok {
		return
	}
	for _, status := range e.evaluate(sample) {
		if e.emit != nil {
			e.emit(status)
		}
	}
}

func (e *qosEstimator) floorSample(sample qosSample) (qosSample, bool) {
	if sample.DataExpected >= e.cfg.SampleFloor {
		if pending, ok := e.pending[sample.DataKind]; ok && qosSamplesCompatible(pending, sample) {
			delete(e.pending, sample.DataKind)
			return mergeQoSSamples(pending, sample), true
		}
		delete(e.pending, sample.DataKind)
		return sample, true
	}

	pending, ok := e.pending[sample.DataKind]
	if !ok || !qosSamplesCompatible(pending, sample) || qosSampleGapTooLarge(pending, sample, e.cfg.Sustain) {
		pending = sample
	} else {
		pending = mergeQoSSamples(pending, sample)
	}
	if pending.DataExpected < e.cfg.SampleFloor {
		e.pending[sample.DataKind] = pending
		return qosSample{}, false
	}
	delete(e.pending, sample.DataKind)
	return pending, true
}

func (e *qosEstimator) evaluate(sample qosSample) []qosStatus {
	var out []qosStatus
	now := sample.At
	if now.IsZero() {
		now = time.Now()
	}
	if status, ok := e.evaluateLimited(sample, now); ok && e.shouldEmit(status.Kind, status.Reason, now) {
		out = append(out, status)
	}
	if status, ok := e.evaluateBacklogged(sample, now); ok && e.shouldEmit(status.Kind, status.Reason, now) {
		out = append(out, status)
	}
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
		return qosStatus{}, false
	}
	if loss <= qosLossEnter {
		return qosStatus{}, false
	}
	if !e.sustained(e.limitedSince, sample.DataKind, now) {
		return qosStatus{}, false
	}
	return qosStatus{
		Kind:         sample.DataKind,
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration),
	}, true
}

func (e *qosEstimator) evaluateBacklogged(sample qosSample, now time.Time) (qosStatus, bool) {
	if sample.Lag <= 0 || !qosKnownKind(sample.RepairKind) || sample.RepairKind == sample.DataKind {
		delete(e.backlogSince, sample.DataKind)
		return qosStatus{}, false
	}
	base, hasBase := e.lagBaseline[sample.DataKind]
	if !hasBase || sample.Lag < base {
		e.lagBaseline[sample.DataKind] = sample.Lag
		base = sample.Lag
	}
	if sample.Lag <= base+e.cfg.LagSlack {
		delete(e.backlogSince, sample.DataKind)
		return qosStatus{}, false
	}
	if !e.sustained(e.backlogSince, sample.DataKind, now) {
		return qosStatus{}, false
	}
	return qosStatus{
		Kind:         sample.DataKind,
		Reason:       protocol.LinkStatusReasonBacklogged,
		DeliveredBps: deliveredBps(sample.DataBytes+sample.RecoveredBytes, sample.Duration),
	}, true
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

func (e *qosEstimator) sustained(m map[transport.Kind]time.Time, kind transport.Kind, now time.Time) bool {
	since, ok := m[kind]
	if !ok {
		m[kind] = now
		return false
	}
	return now.Sub(since) >= e.cfg.Sustain
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
	clear(e.pending)
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

func qosSamplesCompatible(a, b qosSample) bool {
	return a.DataKind == b.DataKind && a.RepairKind == b.RepairKind
}

func qosSampleGapTooLarge(a, b qosSample, maxGap time.Duration) bool {
	if maxGap <= 0 || a.At.IsZero() || b.At.IsZero() {
		return false
	}
	if b.At.Before(a.At) {
		return true
	}
	return b.At.Sub(a.At) > maxGap
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
