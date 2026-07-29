package send

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
)

// laneRuntime is the internal per-lane runtime state (spec 5.3).
//
// In this thin Send design the lane owns only what the send data path needs:
//   - lane id and weight (for the scheduler)
//   - leg (manages underlying UDP+TCP transport state + select + observe; see leg.go)
//   - lane-local FEC transmit window
//
// Lane runtime does NOT own ping/bw logic. Per spec 5.6 the runtime glue
// (recv.Handler) owns the probe/ping and probe/bw instances and drives them
// through Send.WriteFrame. Lane runtime does not own scheduler fairness
// accounting either. The transport lifecycle (active/down/dial/select) lives in
// the leg (leg.go); the lane just embeds one and forwards the data path to it.
type laneRuntime struct {
	id     uint8
	weight atomic.Uint32

	// leg manages the underlying UDP+TCP transport state, selector, and observer
	// (spec 5.2: leg lifecycle). The leg has its own mutex.
	leg leg

	// dialer drives TCP (re)dial off the data path (spec 5.6). nil when this lane
	// has no stream transport configured (UDP-only).
	dialer *dialer

	// tcpReconnectPending is only event-log state. It is set after a TCP leg
	// failure asks the dialer to redial and consumed when the replacement TCP
	// HELLO_ACK makes that leg active again.
	tcpReconnectPending atomic.Bool

	// Per-lane FEC transmit window (spec 9.1).
	fecMu          sync.Mutex
	txWindow       *txSLCWindow
	fecRepairCount uint8
	fecFlushTimer  *time.Timer
	fecFlushArmed  bool
}

func newLaneRuntime(id uint8, weight uint32) *laneRuntime {
	l := &laneRuntime{
		id:             id,
		txWindow:       newTxSLCWindow(maxFECSourceSpan),
		fecRepairCount: 1,
		leg:            newLeg(transport.KindUDP, &selector.QualitySelector{}),
	}
	l.weight.Store(weight)
	return l
}

// Weight returns the lane's scheduling weight (implements schedule.Lane).
func (l *laneRuntime) Weight() uint32 {
	if l == nil {
		return 0
	}
	return l.weight.Load()
}

// ready returns true if the lane has at least one active transport. Readiness is
// driven by leg.active (spec: active默认false, HELLO_ACK/PONG才置true), not by
// "has a ref".
func (l *laneRuntime) ready() bool {
	if l == nil {
		return false
	}
	return l.leg.anyActive()
}

// primaryTransport returns the leg ref for DATA frames (spec 7.1). A zero-Kind
// Ref means no usable transport — the caller drops the packet.
func (l *laneRuntime) primaryTransport() Ref {
	return l.primaryTransportWithQoS(true)
}

func (l *laneRuntime) primaryTransportWithQoS(qosEnabled bool) Ref {
	return l.leg.selectRefWithQoS(rolePrimary, qosEnabled)
}

// shadowTransport returns the leg ref for REPAIR frames (spec 7.2). When only
// one transport is active it returns the same ref as primaryTransport (single-leg
// degradation; DATA and REPAIR share one link). A zero-Kind Ref means drop.
func (l *laneRuntime) shadowTransport() Ref {
	return l.shadowTransportWithQoS(true)
}

func (l *laneRuntime) shadowTransportWithQoS(qosEnabled bool) Ref {
	return l.leg.selectRefWithQoS(roleShadow, qosEnabled)
}

// chooseControlTransport selects the transport for lane-policy control frames
// (WriteFrame with a zero Ref). Control frames follow the primary role.
func (l *laneRuntime) chooseControlTransport() Ref {
	return l.chooseControlTransportWithQoS(true)
}

func (l *laneRuntime) chooseControlTransportWithQoS(qosEnabled bool) Ref {
	return l.leg.selectRefWithQoS(rolePrimary, qosEnabled)
}

// bindUDP records the UDP transport ref (config-injected at lane creation).
// Binding does not mark the transport active; only a peer reply does (spec 6.2).
func (l *laneRuntime) bindUDP(ref Ref) {
	l.leg.bindUDP(ref)
}

// bindTCP records the TCP transport ref after a successful dial (spec 5.6).
func (l *laneRuntime) bindTCP(ref Ref) {
	l.leg.bindTCP(ref)
}

// markActive marks a transport active (spec 6.2: HELLO_ACK/PONG → active). This
// is the lane-facing entry the send-side closures call; the recv glue never
// touches it directly.
func (l *laneRuntime) markActive(kind transport.Kind) {
	l.leg.markActive(kind)
	if debuglog.Enabled() {
		debuglog.Printf("send/leg_state", "active lane=%d kind=%s", l.id, kindEventLabel(kind))
	}
}

// markDown marks a transport inactive (spec 6.2: UDP ping超时 / TCP I/O error).
func (l *laneRuntime) markDown(kind transport.Kind) {
	l.leg.markDown(kind)
	if debuglog.Enabled() {
		debuglog.Printf("send/leg_state", "down lane=%d kind=%s", l.id, kindEventLabel(kind))
	}
}

func (l *laneRuntime) markTCPReconnectPending() {
	if l != nil {
		l.tcpReconnectPending.Store(true)
	}
}

func (l *laneRuntime) consumeTCPReconnectPending() bool {
	if l == nil {
		return false
	}
	return l.tcpReconnectPending.Swap(false)
}

// setPrimary flips the leg's nominal primary orientation. Runtime DATA/REPAIR
// routing is selector-driven; QoS status enters through laneQoSInput below.
func (l *laneRuntime) setPrimary(kind transport.Kind) {
	l.leg.setPrimary(kind)
}

func (l *laneRuntime) setFEC(repairCount uint8) {
	if repairCount == 0 || repairCount > 4 {
		return
	}
	l.fecMu.Lock()
	l.fecRepairCount = repairCount
	l.fecMu.Unlock()
}

func (l *laneRuntime) currentFECRepairCount() uint8 {
	if l == nil {
		return 1
	}
	l.fecMu.Lock()
	defer l.fecMu.Unlock()
	if l.fecRepairCount == 0 {
		return 1
	}
	return l.fecRepairCount
}

type laneQoSInput struct {
	sessionID uint64
	lane      *laneRuntime
}

func (i laneQoSInput) OnQoSStatus(udpLimited bool, udpDeliveredBps uint32, tcpLimited bool, tcpDeliveredBps uint32, repairCount uint8) {
	if i.lane == nil {
		return
	}
	repairBefore := i.lane.currentFECRepairCount()
	i.lane.leg.observeQoSStatus(udpLimited, udpDeliveredBps, tcpLimited, tcpDeliveredBps)
	i.lane.setFEC(repairCount)
	repairAfter := i.lane.currentFECRepairCount()
	metrics.SetGauge(metrics.QoSRepairCount, float64(repairAfter),
		metrics.LU64("session", i.sessionID),
		metrics.LU8("lane", i.lane.id))
	if debuglog.Enabled() {
		primary := i.lane.primaryTransport()
		shadow := i.lane.shadowTransport()
		debuglog.Printf("send/qos", "apply session=%d lane=%d primary={%s} shadow={%s} udp_limited=%t udp_delivered_bps=%d tcp_limited=%t tcp_delivered_bps=%d repair_count=%d repair_before=%d repair_after=%d",
			i.sessionID, i.lane.id, debugLeg(primary), debugLeg(shadow),
			udpLimited, udpDeliveredBps, tcpLimited, tcpDeliveredBps, repairCount, repairBefore, repairAfter)
	}
}

// commitPacket allocates this lane's DATA group/index and optionally retains the
// payload in the FEC transmit window. It returns a completed repair group when
// the window fills, otherwise reports whether the flush timer should be armed.
func (l *laneRuntime) commitPacket(payload []byte, retain bool) (uint32, uint8, txRepairGroup, bool, bool) {
	l.fecMu.Lock()
	defer l.fecMu.Unlock()
	if l.txWindow == nil {
		return 0, 0, txRepairGroup{}, false, false
	}
	wasEmpty := len(l.txWindow.pending) == 0
	groupID, sourceIndex, group, ready := l.txWindow.add(payload, retain)
	if ready {
		l.cancelFECFlushTimerLocked()
		return groupID, sourceIndex, group, true, false
	}
	shouldArmFlush := retain && wasEmpty && len(l.txWindow.pending) > 0
	return groupID, sourceIndex, txRepairGroup{}, false, shouldArmFlush
}

// cancelFECFlushTimerLocked stops the flush timer. Caller must hold fecMu.
func (l *laneRuntime) cancelFECFlushTimerLocked() {
	if l.fecFlushTimer != nil {
		l.fecFlushTimer.Stop()
	}
	l.fecFlushArmed = false
}

func (l *laneRuntime) releaseFEC() {
	l.fecMu.Lock()
	defer l.fecMu.Unlock()
	l.cancelFECFlushTimerLocked()
	if l.txWindow != nil {
		l.txWindow.releaseAll()
	}
}

// txSLCWindow is the per-lane FEC transmit window (spec 9.1).
type txSLCWindow struct {
	sourceCount   int
	nextGroupID   uint32
	activeGroupID uint32
	nextIndex     uint8
	pending       []txSymbol
}

type txSymbol struct {
	packet *packetbuf.Packet
}

// txRepairGroup carries the data shards for one FEC group.
type txRepairGroup struct {
	groupID    uint32
	sourceSpan uint8
	packets    []*packetbuf.Packet
}

func newTxSLCWindow(sourceCount int) *txSLCWindow {
	return &txSLCWindow{sourceCount: sourceCount}
}

func (w *txSLCWindow) add(packet []byte, retain bool) (uint32, uint8, txRepairGroup, bool) {
	if w.sourceCount <= 0 {
		return 0, 0, txRepairGroup{}, false
	}
	// FEC may become active after this lane has already emitted part of a group.
	// Those earlier DATA symbols were not retained, so the first protected DATA
	// starts a fresh group.
	if retain && len(w.pending) == 0 && w.nextIndex != 0 {
		w.nextIndex = 0
	}
	if w.nextIndex == 0 {
		w.activeGroupID = w.nextGroupID
		w.nextGroupID = (w.nextGroupID + 1) & maxTxGroupID
	}
	groupID := w.activeGroupID
	sourceIndex := w.nextIndex
	w.nextIndex++

	if retain {
		pkt := packetbuf.Acquire(len(packet))
		copy(pkt.Payload, packet)
		pkt.SetLen(len(packet))
		w.pending = append(w.pending, txSymbol{packet: pkt})
	}

	if int(w.nextIndex) < w.sourceCount {
		return groupID, sourceIndex, txRepairGroup{}, false
	}
	w.nextIndex = 0
	if !retain {
		return groupID, sourceIndex, txRepairGroup{}, false
	}

	group := txRepairGroup{
		groupID:    groupID,
		sourceSpan: uint8(w.sourceCount),
		packets:    make([]*packetbuf.Packet, w.sourceCount),
	}
	for i := 0; i < w.sourceCount; i++ {
		group.packets[i] = w.pending[i].packet
	}
	w.pending = w.pending[:0]
	return groupID, sourceIndex, group, true
}

func (w *txSLCWindow) flush() (txRepairGroup, bool) {
	if w.sourceCount <= 0 || len(w.pending) == 0 {
		return txRepairGroup{}, false
	}
	if int(w.nextIndex) >= w.sourceCount {
		return txRepairGroup{}, false
	}
	count := len(w.pending)
	group := txRepairGroup{
		groupID:    w.activeGroupID,
		sourceSpan: uint8(count),
		packets:    make([]*packetbuf.Packet, count),
	}
	for i := 0; i < count; i++ {
		group.packets[i] = w.pending[i].packet
	}
	w.pending = w.pending[:0]
	w.nextIndex = 0
	return group, true
}

func (w *txSLCWindow) releaseAll() {
	for i := range w.pending {
		w.pending[i].packet.Release()
	}
	w.pending = w.pending[:0]
	w.nextIndex = 0
}
