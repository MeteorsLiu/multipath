package recv

import (
	"math"
	"slices"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultQoSWindow      = 3 * time.Second
	defaultQoSSustain     = 3 * time.Second
	defaultQoSSampleFloor = 100
	defaultQoSLagSlack    = 300 * time.Millisecond

	qosLossEnter = 0.05
	qosLossExit  = 0.01
)

type qosConfig struct {
	Window      time.Duration
	Sustain     time.Duration
	SampleFloor uint64
	LagSlack    time.Duration
	Now         func() time.Time
}

type qosStatus struct {
	LegKind      uint8
	Reason       uint8
	DeliveredBps uint32
}

type qosWindow struct {
	cfg qosConfig

	events []qosEvent
	groups map[uint32]*qosGroup

	limitedSince map[transport.Kind]time.Time
	backlogSince map[transport.Kind]time.Time
	lagBaseline  map[transport.Kind]time.Duration
	last         qosSnapshot
}

type qosEvent struct {
	at       time.Time
	kind     transport.Kind
	category frameCategory
	packetID uint32
	baseID   uint32
	span     uint8
	bytes    int
}

type qosGroup struct {
	baseID     uint32
	span       uint8
	repairKind transport.Kind
	repairAt   time.Time
	dataKind   transport.Kind
	lastDataAt time.Time
	dataSeen   uint8
}

func newQoSWindow(cfg qosConfig) *qosWindow {
	if cfg.Window <= 0 {
		cfg.Window = defaultQoSWindow
	}
	if cfg.Sustain <= 0 {
		cfg.Sustain = defaultQoSSustain
	}
	if cfg.SampleFloor == 0 {
		cfg.SampleFloor = defaultQoSSampleFloor
	}
	if cfg.LagSlack <= 0 {
		cfg.LagSlack = defaultQoSLagSlack
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &qosWindow{
		cfg:          cfg,
		groups:       make(map[uint32]*qosGroup),
		limitedSince: make(map[transport.Kind]time.Time),
		backlogSince: make(map[transport.Kind]time.Time),
		lagBaseline:  make(map[transport.Kind]time.Duration),
	}
}

func (w *qosWindow) ObserveData(kind transport.Kind, packetID uint32, bytes int, at time.Time) {
	if !qosKnownKind(kind) {
		return
	}
	w.events = append(w.events, qosEvent{
		at:       at,
		kind:     kind,
		category: catData,
		packetID: packetID,
		bytes:    bytes,
	})

	base := packetID - packetID%maxFECSourceSpan
	g := w.groupFor(base)
	if g.span == 0 {
		g.span = maxFECSourceSpan
	}
	g.dataKind = kind
	if at.After(g.lastDataAt) {
		g.lastDataAt = at
	}
	g.dataSeen++
}

func (w *qosWindow) ObserveDataAndEvaluate(kind transport.Kind, packetID uint32, bytes int, at time.Time) []qosStatus {
	w.ObserveData(kind, packetID, bytes, at)
	return w.Evaluate(at)
}

func (w *qosWindow) ObserveRepair(kind transport.Kind, basePacketID uint32, span uint8, bytes int, at time.Time) {
	if !qosKnownKind(kind) || span == 0 || span > maxFECSourceSpan {
		return
	}
	w.events = append(w.events, qosEvent{
		at:       at,
		kind:     kind,
		category: catRepair,
		baseID:   basePacketID,
		span:     span,
		bytes:    bytes,
	})

	g := w.groupFor(basePacketID)
	g.span = span
	g.repairKind = kind
	g.repairAt = at
}

func (w *qosWindow) ObserveRepairAndEvaluate(kind transport.Kind, basePacketID uint32, span uint8, bytes int, at time.Time) []qosStatus {
	w.ObserveRepair(kind, basePacketID, span, bytes, at)
	return w.Evaluate(at)
}

func (w *qosWindow) Evaluate(now time.Time) []qosStatus {
	w.prune(now)

	s := w.snapshot(now)
	if s.expectedData < w.cfg.SampleFloor {
		if !w.hasPendingState() || w.last.expectedData < w.cfg.SampleFloor {
			w.resetTransientState()
			return nil
		}
		s = w.last
	} else {
		w.last = s
	}

	var out []qosStatus
	for _, kind := range []transport.Kind{transport.KindUDP, transport.KindTCP} {
		if status, ok := w.evaluateLimited(kind, s, now); ok {
			out = append(out, status)
			continue
		}
		if status, ok := w.evaluateBacklogged(kind, s, now); ok {
			out = append(out, status)
		}
	}
	return out
}

func (w *qosWindow) groupFor(base uint32) *qosGroup {
	g := w.groups[base]
	if g == nil {
		g = &qosGroup{baseID: base}
		w.groups[base] = g
	}
	return g
}

type qosSnapshot struct {
	expectedData uint64
	dataGot      map[transport.Kind]uint64
	bytes        map[transport.Kind]uint64
	lags         map[transport.Kind][]time.Duration
}

func (w *qosWindow) snapshot(now time.Time) qosSnapshot {
	s := qosSnapshot{
		dataGot: make(map[transport.Kind]uint64),
		bytes:   make(map[transport.Kind]uint64),
		lags:    make(map[transport.Kind][]time.Duration),
	}

	var highWater uint32
	var haveWater bool
	for _, ev := range w.events {
		s.bytes[ev.kind] += uint64(maxInt(ev.bytes, 0))
		switch ev.category {
		case catData:
			s.dataGot[ev.kind]++
			if !haveWater || ev.packetID > highWater {
				highWater = ev.packetID
				haveWater = true
			}
		case catRepair:
			last := ev.baseID + uint32(ev.span) - 1
			if !haveWater || last > highWater {
				highWater = last
				haveWater = true
			}
		}
	}
	if haveWater {
		s.expectedData = uint64(highWater) + 1
	}

	for _, g := range w.groups {
		if g.repairAt.IsZero() || g.lastDataAt.IsZero() || g.dataKind == 0 || g.repairKind == g.dataKind {
			continue
		}
		lag := g.lastDataAt.Sub(g.repairAt)
		if lag < 0 {
			continue
		}
		s.lags[g.dataKind] = append(s.lags[g.dataKind], lag)
	}

	return s
}

func (w *qosWindow) evaluateLimited(kind transport.Kind, s qosSnapshot, now time.Time) (qosStatus, bool) {
	got := s.dataGot[kind]
	if got == 0 {
		delete(w.limitedSince, kind)
		return qosStatus{}, false
	}
	var loss float64
	if got < s.expectedData {
		loss = float64(s.expectedData-got) / float64(s.expectedData)
	}
	if loss < qosLossExit {
		delete(w.limitedSince, kind)
		return qosStatus{}, false
	}
	if loss <= qosLossEnter {
		return qosStatus{}, false
	}
	if !w.sustained(w.limitedSince, kind, now) {
		return qosStatus{}, false
	}
	return qosStatus{
		LegKind:      qosProtocolLeg(kind),
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: w.deliveredBps(s.bytes[kind]),
	}, true
}

func (w *qosWindow) evaluateBacklogged(kind transport.Kind, s qosSnapshot, now time.Time) (qosStatus, bool) {
	lag, ok := medianDuration(s.lags[kind])
	if !ok {
		delete(w.backlogSince, kind)
		return qosStatus{}, false
	}
	base, hasBase := w.lagBaseline[kind]
	if hasBase && lag < base {
		w.lagBaseline[kind] = lag
		base = lag
	}
	if lag <= base+w.cfg.LagSlack {
		delete(w.backlogSince, kind)
		return qosStatus{}, false
	}
	if !w.sustained(w.backlogSince, kind, now) {
		return qosStatus{}, false
	}
	return qosStatus{
		LegKind:      qosProtocolLeg(kind),
		Reason:       protocol.LinkStatusReasonBacklogged,
		DeliveredBps: w.deliveredBps(s.bytes[kind]),
	}, true
}

func (w *qosWindow) sustained(m map[transport.Kind]time.Time, kind transport.Kind, now time.Time) bool {
	since, ok := m[kind]
	if !ok {
		m[kind] = now
		return false
	}
	return now.Sub(since) >= w.cfg.Sustain
}

func (w *qosWindow) deliveredBps(bytes uint64) uint32 {
	if w.cfg.Window <= 0 {
		return 0
	}
	bps := bytes * 8 * uint64(time.Second) / uint64(w.cfg.Window)
	if bps > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(bps)
}

func (w *qosWindow) prune(now time.Time) {
	cutoff := now.Add(-w.cfg.Window)
	keep := w.events[:0]
	for _, ev := range w.events {
		if ev.at.Before(cutoff) {
			continue
		}
		keep = append(keep, ev)
	}
	w.events = keep

	for base, g := range w.groups {
		if (!g.repairAt.IsZero() && g.repairAt.Before(cutoff)) &&
			(!g.lastDataAt.IsZero() && g.lastDataAt.Before(cutoff)) {
			delete(w.groups, base)
		}
	}
}

func (w *qosWindow) resetTransientState() {
	clear(w.limitedSince)
	clear(w.backlogSince)
}

func (w *qosWindow) hasPendingState() bool {
	return len(w.limitedSince) > 0 || len(w.backlogSince) > 0
}

func medianDuration(values []time.Duration) (time.Duration, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := append([]time.Duration(nil), values...)
	slices.Sort(sorted)
	return sorted[len(sorted)/2], true
}

func qosKnownKind(kind transport.Kind) bool {
	return kind == transport.KindUDP || kind == transport.KindTCP
}

func qosProtocolLeg(kind transport.Kind) uint8 {
	switch kind {
	case transport.KindUDP:
		return protocol.LinkStatusLegUDP
	case transport.KindTCP:
		return protocol.LinkStatusLegTCP
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
