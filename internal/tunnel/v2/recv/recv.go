// Package recv is the v2 receive side. It decodes transport-bound frames,
// handles DATA and REPAIR locally with per-lane FEC receive windows and a
// session-scoped emit dedupe, and dispatches the seven control frame types to
// a recv.Handler (spec 5.5/5.6). It also maintains a reserved per-lane QoS
// arrival ledger (spec 8.3/8.4) used by later FEC-differential detection.
//
// recv imports neither send nor the probe packages; the runtime glue that wires
// Recv to Send.WriteFrame lives elsewhere.
package recv

import (
	"context"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
)

const (
	defaultPacketQueueSize = 1024
	maxFECSourceSpan       = 4
)

// Handler receives the control frames Recv does not handle locally (spec 5.6).
// DATA and REPAIR never reach a Handler. The from argument is the observed
// transport (spec design name Ref; an alias of transport.LegRef).
type Handler interface {
	OnHello(ctx context.Context, from Ref, frame protocol.Frame) error
	OnHelloAck(ctx context.Context, from Ref, frame protocol.Frame) error
	OnPing(ctx context.Context, from Ref, frame protocol.Frame) error
	OnPong(ctx context.Context, from Ref, frame protocol.Frame) error
	OnClose(ctx context.Context, from Ref, frame protocol.Frame) error
	OnBandwidthProbe(ctx context.Context, from Ref, frame protocol.Frame) error
	OnBandwidthProbeAck(ctx context.Context, from Ref, frame protocol.Frame) error
}

type Config struct {
	Handler        Handler
	SessionManager *sessionpkg.Manager
}

// Recv decodes transport-bound frames into IP packets and dispatches control
// frames to its Handler.
//
// Locking convention:
//
//   - statesMu (RWMutex) protects the states map and is also held while calling
//     sessionManager.Get/Delete to keep the "session admitted iff state
//     present" invariant under concurrent close.
//   - recvState.mu protects the per-session windows, accounting, and closed
//     flag. It is taken AFTER releasing statesMu, never the other way around.
type Recv struct {
	statesMu  sync.RWMutex
	handler   Handler
	manager   *sessionpkg.Manager
	fecCodecs [maxFECSourceSpan + 1]fecCodec
	packets   chan *packetbuf.Packet
	states    map[*sessionpkg.Session]*recvState
}

type recvState struct {
	mu     sync.Mutex
	closed bool

	// rxWindows holds one FEC receive window per lane id. DATA and REPAIR select
	// their window by frame.LaneID, so reconstruction never crosses lanes
	// (spec 9.2).
	rxWindows map[uint8]*rxSLCWindow

	// dedupe is the session-scoped emit dedupe (spec 8.2). It decides whether an
	// original or FEC-recovered packet id has already been written to TUN.
	dedupe *emitDedupe

	// accounting holds the per-lane QoS arrival ledger (spec 8.3/8.4). It is
	// written on every wire arrival but read by nothing this round; not exported
	// (spec 8.1).
	accounting map[uint8]*laneArrivalStats

	// shardScratch is reused by recoverPacket to materialize the shard slice for
	// fec.Reconstruct without per-call allocation.
	shardScratch [8][]byte
}

// windowFor returns the per-lane FEC receive window for laneID, creating it on
// first use. Caller holds s.mu.
func (s *recvState) windowFor(laneID uint8) *rxSLCWindow {
	w := s.rxWindows[laneID]
	if w == nil {
		w = newRxSLCWindow(4)
		s.rxWindows[laneID] = w
	}
	return w
}

// statsFor returns the per-lane arrival ledger for laneID, creating it on first
// use. Caller holds s.mu.
func (s *recvState) statsFor(laneID uint8) *laneArrivalStats {
	st := s.accounting[laneID]
	if st == nil {
		st = &laneArrivalStats{}
		s.accounting[laneID] = st
	}
	return st
}

type fecCodec interface {
	Reconstruct(shards [][]byte, key uint16) error
}

func New(configs ...Config) *Recv {
	out := &Recv{
		states:  make(map[*sessionpkg.Session]*recvState),
		packets: make(chan *packetbuf.Packet, defaultPacketQueueSize),
		manager: &sessionpkg.Manager{},
	}
	for sourceSpan := 1; sourceSpan <= maxFECSourceSpan; sourceSpan++ {
		out.fecCodecs[sourceSpan], _ = fecpkg.NewCodec(sourceSpan, 1)
	}
	for _, cfg := range configs {
		if cfg.Handler != nil {
			out.handler = cfg.Handler
		}
		if cfg.SessionManager != nil {
			out.manager = cfg.SessionManager
		}
	}
	return out
}

// Packets exposes the TUN-bound IP packet stream. The caller owns each packet
// and must Release it after writing to TUN.
func (o *Recv) Packets() <-chan *packetbuf.Packet {
	return o.packets
}

func (o *Recv) Write(ctx context.Context, packet *packetbuf.Packet) error {
	return o.WriteTo(ctx, Ref{}, packet)
}

// WriteTo decodes one transport-bound packet observed on leg and routes it.
func (o *Recv) WriteTo(ctx context.Context, leg Ref, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	releaseEventPacket := true
	defer func() {
		if releaseEventPacket {
			packet.Release()
		}
	}()

	frame, err := protocol.Decode(packet.Payload)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "decode_drop leg={%s} bytes=%d err=%v", debugLeg(leg), len(packet.Payload), err)
		}
		metrics.IncCounter(metrics.ProtocolDecodeErrorsTotal,
			metrics.L("leg", kindMetricLabel(leg.Kind)),
		)
		return nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "frame_in %s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
	}
	metrics.IncCounter(metrics.ProtocolFramesTotal,
		metrics.LStr("direction", "rx"),
		metrics.LStr("type", debugFrameType(frame.Type)),
		metrics.LU64("session", frame.SessionID),
		metrics.LU8("lane", frame.LaneID),
		metrics.LStr("leg", kindMetricLabel(leg.Kind)),
	)

	switch frame.Type {
	case protocol.TypeHELLO:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeHELLOACK:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypePING:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypePONG:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeDATA:
		consumed, err := o.handleDATA(ctx, leg, frame, packet)
		if consumed {
			releaseEventPacket = false
		}
		return err
	case protocol.TypeREPAIR:
		return o.handleREPAIR(ctx, leg, frame)
	case protocol.TypeCLOSE:
		return o.handleCLOSE(ctx, leg, frame)
	case protocol.TypeBandwidthProbe:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeBandwidthProbeAck:
		return o.handleControl(ctx, leg, frame)
	default:
		return nil
	}
}

// handleControl validates a control frame's body matches its type, then
// dispatches it to the matching Handler method. DATA and REPAIR never reach
// here. A nil Handler drops the frame.
func (o *Recv) handleControl(ctx context.Context, leg Ref, frame protocol.Frame) error {
	if err := validateControlBody(frame); err != nil {
		return err
	}
	if o.handler == nil {
		return nil
	}
	switch frame.Type {
	case protocol.TypeHELLO:
		return o.handler.OnHello(ctx, leg, frame)
	case protocol.TypeHELLOACK:
		return o.handler.OnHelloAck(ctx, leg, frame)
	case protocol.TypePING:
		return o.handler.OnPing(ctx, leg, frame)
	case protocol.TypePONG:
		return o.handler.OnPong(ctx, leg, frame)
	case protocol.TypeBandwidthProbe:
		return o.handler.OnBandwidthProbe(ctx, leg, frame)
	case protocol.TypeBandwidthProbeAck:
		return o.handler.OnBandwidthProbeAck(ctx, leg, frame)
	default:
		return nil
	}
}

// validateControlBody returns ErrInvalidFrame when the decoded body type does
// not match the frame type.
func validateControlBody(frame protocol.Frame) error {
	var ok bool
	switch frame.Type {
	case protocol.TypeHELLO:
		_, ok = frame.Body.(protocol.HelloBody)
	case protocol.TypeHELLOACK:
		_, ok = frame.Body.(protocol.HelloAckBody)
	case protocol.TypePING, protocol.TypePONG:
		_, ok = frame.Body.(protocol.PingBody)
	case protocol.TypeBandwidthProbe:
		_, ok = frame.Body.(protocol.BandwidthProbeBody)
	case protocol.TypeBandwidthProbeAck:
		_, ok = frame.Body.(protocol.BandwidthProbeAckBody)
	default:
		ok = true
	}
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return nil
}

// handleDATA implements spec 8.3. It returns true when ownership of packet was
// transferred to the TUN output channel (zero-copy emit).
func (o *Recv) handleDATA(ctx context.Context, leg Ref, frame protocol.Frame, packet *packetbuf.Packet) (bool, error) {
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		return false, protocol.ErrInvalidFrame
	}

	state := o.recvState(frame.SessionID)
	if state == nil {
		return false, nil
	}

	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return false, nil
	}
	if !state.dedupe.mark(body.PacketID) {
		// Duplicate: drop before window and before TUN; do NOT account (spec 8.3).
		state.mu.Unlock()
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop_duplicate session=%d packet_id=%d", frame.SessionID, body.PacketID)
		}
		return false, nil
	}
	// First arrival: record link-delivery accounting, then store in the window.
	state.statsFor(frame.LaneID).recordArrival(leg.Kind, catData, len(body.Packet))
	recoverable, recoverableOK := state.windowFor(frame.LaneID).addData(body.PacketID, body.Packet)
	state.mu.Unlock()

	consumed, err := o.emitTransportPacket(ctx, packet, body.Packet)
	if err != nil {
		return false, err
	}
	if recoverableOK {
		return consumed, o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, recoverable)
	}
	return consumed, nil
}

// handleREPAIR implements spec 8.4.
func (o *Recv) handleREPAIR(ctx context.Context, leg Ref, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	if body.SourceSpan == 0 || body.SourceSpan > maxFECSourceSpan {
		return nil
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_accept"),
		metrics.L("session", frame.SessionID),
		metrics.L("source_span", body.SourceSpan),
	)

	state := o.recvState(frame.SessionID)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return nil
	}
	// REPAIR has no dedupe identity; account it on arrival (spec 8.4).
	state.statsFor(frame.LaneID).recordArrival(leg.Kind, catRepair, len(body.Symbol))
	recoverable, ok := state.windowFor(frame.LaneID).addRepair(body.BasePacketID, body.Key, int(body.SourceSpan), body.Symbol)
	state.mu.Unlock()
	if ok {
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_recoverable"),
			metrics.L("session", frame.SessionID),
			metrics.L("source_span", recoverable.sourceSpan),
		)
		return o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, recoverable)
	}
	return nil
}

func (o *Recv) handleCLOSE(ctx context.Context, leg Ref, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	var session *sessionpkg.Session
	if body.Scope == protocol.CloseScopeSession {
		session, _ = o.manager.Get(frame.SessionID)
	}
	if o.handler != nil {
		if err := o.handler.OnClose(ctx, leg, frame); err != nil {
			return err
		}
	}
	if body.Scope == protocol.CloseScopeSession {
		o.closeRecvState(frame.SessionID, session)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, sessionID uint64, laneID uint8, state *recvState, recoverable rxRecoverable) error {
	codec := o.fecCodecForSourceSpan(recoverable.sourceSpan)
	if state == nil || codec == nil {
		return nil
	}
	pkt, ok := o.recoverPacket(sessionID, laneID, state, recoverable, codec)
	if !ok {
		return nil
	}
	select {
	case o.packets <- pkt:
		return nil
	case <-ctx.Done():
		pkt.Release()
		return ctx.Err()
	}
}

func (o *Recv) recoverPacket(sessionID uint64, laneID uint8, state *recvState, recoverable rxRecoverable, codec fecCodec) (*packetbuf.Packet, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, false
	}
	window := state.rxWindows[laneID]
	if window == nil {
		return nil, false
	}
	shards, ok := window.buildShardsLocked(recoverable, state.shardScratch[:0])
	if !ok {
		return nil, false
	}
	if err := codec.Reconstruct(shards, recoverable.key); err != nil {
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "recover_err"),
			metrics.L("session", sessionID),
			metrics.L("source_span", recoverable.sourceSpan),
		)
		return nil, false
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "reconstruct_done"),
		metrics.L("session", sessionID),
		metrics.L("source_span", recoverable.sourceSpan),
	)

	packetID := recoverable.basePacketID + uint32(recoverable.missingIndex)
	reconstructed := shards[recoverable.missingIndex]
	payload, ipOK := recoveredIPv4Packet(reconstructed)
	if !ipOK {
		return nil, false
	}
	// Record existence in the window, then run the recovered id through the
	// session emit dedupe so a late original is not double-emitted (spec 8.3).
	// Recovered DATA is internal reconstruction, not a wire arrival, so it is
	// NOT accounted.
	window.addData(packetID, payload)
	if !state.dedupe.mark(packetID) {
		return nil, false
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "recover_emit"),
		metrics.L("session", sessionID),
		metrics.L("source_span", recoverable.sourceSpan),
	)
	pkt := packetbuf.Acquire(len(payload))
	copy(pkt.Payload, payload)
	return pkt, true
}

func (o *Recv) fecCodecForSourceSpan(sourceSpan int) fecCodec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return nil
	}
	return o.fecCodecs[sourceSpan]
}

func (o *Recv) emitTransportPacket(ctx context.Context, packet *packetbuf.Packet, payload []byte) (bool, error) {
	packet.Payload = payload
	select {
	case o.packets <- packet:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (o *Recv) recvState(sessionID uint64) *recvState {
	o.statesMu.RLock()
	session, ok := o.manager.Get(sessionID)
	if !ok {
		o.statesMu.RUnlock()
		return nil
	}
	state := o.states[session]
	o.statesMu.RUnlock()
	if state != nil {
		return state
	}

	o.statesMu.Lock()
	defer o.statesMu.Unlock()
	session, ok = o.manager.Get(sessionID)
	if !ok {
		return nil
	}
	if state := o.states[session]; state != nil {
		return state
	}
	state = &recvState{
		rxWindows:  make(map[uint8]*rxSLCWindow),
		dedupe:     newEmitDedupe(0),
		accounting: make(map[uint8]*laneArrivalStats),
	}
	o.states[session] = state
	debuglog.Printf("recv", "session_create session=%d", sessionID)
	return state
}

func (o *Recv) closeRecvState(sessionID uint64, session *sessionpkg.Session) {
	o.statesMu.Lock()
	if session == nil {
		session, _ = o.manager.Get(sessionID)
	}
	var state *recvState
	if session != nil {
		state = o.states[session]
		delete(o.states, session)
	}
	o.manager.Delete(sessionID)
	o.statesMu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.closed = true
	for _, window := range state.rxWindows {
		window.releaseAll()
	}
	state.mu.Unlock()
}

func recoveredIPv4Packet(packet []byte) ([]byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil, false
	}
	headerLen := int(packet[0]&0x0f) * 4
	totalLen := int(packet[2])<<8 | int(packet[3])
	if headerLen < 20 || totalLen < headerLen || totalLen > len(packet) {
		return nil, false
	}
	return packet[:totalLen], true
}
