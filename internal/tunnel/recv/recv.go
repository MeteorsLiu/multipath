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
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultPacketQueueSize = 1024
	maxFECSourceSpan       = 4
)

type Handler interface {
	OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
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
//   - statesMu (RWMutex) protects the states map and is also held while
//     calling sessionManager.Get/Delete to keep the "session admitted iff
//     state present" invariant under concurrent close.
//   - recvState.mu protects the per-session receive window and closed flag.
//     It is taken AFTER releasing statesMu, never the other way around.
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

	// rxWindows holds one FEC receive window per lane id. DATA and REPAIR
	// select their window by frame.LaneID, so FEC reconstruction never crosses
	// lane boundaries.
	rxWindows map[uint8]*rxSLCWindow

	// dedupe is the session-scoped emit dedupe. It decides whether an
	// original or FEC-recovered packet id has already been written to TUN; it
	// is intentionally separate from the FEC receive windows.
	dedupe *emitDedupe

	// shardScratch is reused by recoverPacket to materialize the shard
	// slice for fec.Reconstruct without per-call allocation. Size is fixed
	// at the maximum supported FEC group (sourceCount + 1 repair).
	shardScratch [8][]byte
}

// windowFor returns the per-lane FEC receive window for laneID, creating it on
// first use. Callers must hold s.mu.
func (s *recvState) windowFor(laneID uint8) *rxSLCWindow {
	w := s.rxWindows[laneID]
	if w == nil {
		w = newRxSLCWindow(4)
		s.rxWindows[laneID] = w
	}
	return w
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

func (o *Recv) Packets() <-chan *packetbuf.Packet {
	return o.packets
}

func (o *Recv) Write(ctx context.Context, packet *packetbuf.Packet) error {
	return o.WriteTo(ctx, transport.LegRef{}, packet)
}

func (o *Recv) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error {
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
		return o.handleHELLO(ctx, leg, frame)
	case protocol.TypeHELLOACK:
		return o.handleHELLOACK(ctx, leg, frame)
	case protocol.TypePING:
		return o.handlePING(ctx, leg, frame)
	case protocol.TypePONG:
		return o.handlePONG(ctx, leg, frame)
	case protocol.TypeDATA:
		consumed, err := o.handleDATA(ctx, frame, packet)
		if consumed {
			releaseEventPacket = false
		}
		return err
	case protocol.TypeREPAIR:
		return o.handleREPAIR(ctx, frame)
	case protocol.TypeCLOSE:
		return o.handleCLOSE(ctx, leg, frame)
	case protocol.TypeBandwidthProbe:
		return o.handleBandwidthProbe(ctx, leg, frame)
	case protocol.TypeBandwidthProbeAck:
		return o.handleBandwidthProbeAck(ctx, leg, frame)
	default:
		return nil
	}
}

func (o *Recv) handleHELLO(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.HelloBody); !ok {
		debuglog.Printf("recv/control", "hello_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "hello session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "hello_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnHello(ctx, leg, frame)
}

func (o *Recv) handleHELLOACK(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.HelloAckBody); !ok {
		debuglog.Printf("recv/control", "hello_ack_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "hello_ack session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "hello_ack_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnHelloAck(ctx, leg, frame)
}

func (o *Recv) handlePING(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.PingBody); !ok {
		debuglog.Printf("recv/control", "ping_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "ping session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "ping_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnPing(ctx, leg, frame)
}

func (o *Recv) handlePONG(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.PingBody); !ok {
		debuglog.Printf("recv/control", "pong_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "pong session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "pong_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnPong(ctx, leg, frame)
}

func (o *Recv) handleBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.BandwidthProbeBody); !ok {
		debuglog.Printf("recv/control", "bw_probe_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "bw_probe session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "bw_probe_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnBandwidthProbe(ctx, leg, frame)
}

func (o *Recv) handleBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.BandwidthProbeAckBody); !ok {
		debuglog.Printf("recv/control", "bw_probe_ack_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "bw_probe_ack session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "bw_probe_ack_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.handler.OnBandwidthProbeAck(ctx, leg, frame)
}

func (o *Recv) handleDATA(ctx context.Context, frame protocol.Frame, packet *packetbuf.Packet) (bool, error) {
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		}
		return false, protocol.ErrInvalidFrame
	}

	state := o.recvState(frame.SessionID)
	if state == nil {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop missing_session session=%d lane=%d packet_id=%d", frame.SessionID, frame.LaneID, body.PacketID)
		}
		return false, nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "data_accept session=%d lane=%d packet_id=%d bytes=%d", frame.SessionID, frame.LaneID, body.PacketID, len(body.Packet))
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop closed_session session=%d lane=%d packet_id=%d", frame.SessionID, frame.LaneID, body.PacketID)
		}
		return false, nil
	}
	if !state.dedupe.mark(body.PacketID) {
		state.mu.Unlock()
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop_duplicate session=%d packet_id=%d", frame.SessionID, body.PacketID)
		}
		return false, nil
	}
	recoverable, recoverableOK := state.windowFor(frame.LaneID).addData(body.PacketID, body.Packet)
	state.mu.Unlock()
	if debuglog.Enabled() {
		debuglog.Printf("recv", "data_fec_window session=%d packet_id=%d recoverable=%t", frame.SessionID, body.PacketID, recoverableOK)
	}
	consumed, err := o.emitTransportPacket(ctx, packet, body.Packet)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_emit_err session=%d packet_id=%d err=%v", frame.SessionID, body.PacketID, err)
		}
		return false, err
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "data_emit session=%d packet_id=%d consumed=%t bytes=%d", frame.SessionID, body.PacketID, consumed, len(body.Packet))
	}
	if recoverableOK {
		return consumed, o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, recoverable)
	}
	return consumed, nil
}

func (o *Recv) handleREPAIR(ctx context.Context, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		debuglog.Printf("recv", "repair_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	if body.SourceSpan == 0 || body.SourceSpan > maxFECSourceSpan {
		debuglog.Printf("recv", "repair_drop invalid_source_span session=%d lane=%d base_packet_id=%d key=%d source_span=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, body.SourceSpan)
		return nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "repair_accept session=%d lane=%d base_packet_id=%d key=%d source_span=%d symbol_len=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, body.SourceSpan, len(body.Symbol))
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_accept"),
		metrics.L("session", frame.SessionID),
		metrics.L("source_span", body.SourceSpan),
	)

	state := o.recvState(frame.SessionID)
	if state == nil {
		debuglog.Printf("recv", "repair_drop missing_session session=%d lane=%d base_packet_id=%d key=%d source_span=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, body.SourceSpan)
		return nil
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		debuglog.Printf("recv", "repair_drop closed_session session=%d lane=%d base_packet_id=%d key=%d source_span=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, body.SourceSpan)
		return nil
	}
	if recoverable, ok := state.windowFor(frame.LaneID).addRepair(body.BasePacketID, body.Key, int(body.SourceSpan), body.Symbol); ok {
		state.mu.Unlock()
		debuglog.Printf("recv", "repair_recoverable session=%d base_packet_id=%d key=%d source_span=%d missing_index=%d", frame.SessionID, recoverable.basePacketID, recoverable.key, recoverable.sourceSpan, recoverable.missingIndex)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_recoverable"),
			metrics.L("session", frame.SessionID),
			metrics.L("source_span", recoverable.sourceSpan),
		)
		return o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, recoverable)
	}
	state.mu.Unlock()
	debuglog.Printf("recv", "repair_stored session=%d base_packet_id=%d key=%d source_span=%d", frame.SessionID, body.BasePacketID, body.Key, body.SourceSpan)
	return nil
}

func (o *Recv) handleCLOSE(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		debuglog.Printf("recv", "close_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	var session *sessionpkg.Session
	if body.Scope == protocol.CloseScopeSession {
		session, _ = o.manager.Get(frame.SessionID)
	}
	debuglog.Printf("recv/control", "close session=%d lane=%d scope=%d leg={%s}", frame.SessionID, frame.LaneID, body.Scope, debugLeg(leg))
	if o.handler == nil {
		debuglog.Printf("recv/control", "close_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
	} else if err := o.handler.OnClose(ctx, leg, frame); err != nil {
		return err
	}
	if body.Scope == protocol.CloseScopeSession {
		o.closeRecvState(frame.SessionID, session)
		debuglog.Printf("recv", "close_session session=%d", frame.SessionID)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, sessionID uint64, laneID uint8, state *recvState, recoverable rxRecoverable) error {
	codec := o.fecCodecForSourceSpan(recoverable.sourceSpan)
	if state == nil || codec == nil {
		debuglog.Printf("recv", "recover_skip session_nil=%t fec_nil=%t base_packet_id=%d key=%d source_span=%d missing_index=%d", state == nil, codec == nil, recoverable.basePacketID, recoverable.key, recoverable.sourceSpan, recoverable.missingIndex)
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
		debuglog.Printf("recv", "recover_drop closed_session base_packet_id=%d key=%d source_span=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.sourceSpan, recoverable.missingIndex)
		return nil, false
	}
	window := state.rxWindows[laneID]
	if window == nil {
		debuglog.Printf("recv", "recover_drop missing_window lane=%d base_packet_id=%d key=%d", laneID, recoverable.basePacketID, recoverable.key)
		return nil, false
	}
	shards, ok := window.buildShardsLocked(recoverable, state.shardScratch[:0])
	if !ok {
		debuglog.Printf("recv", "recover_drop window_stale base_packet_id=%d key=%d source_span=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.sourceSpan, recoverable.missingIndex)
		return nil, false
	}
	if err := codec.Reconstruct(shards, recoverable.key); err != nil {
		debuglog.Printf("recv", "recover_err base_packet_id=%d key=%d source_span=%d missing_index=%d err=%v", recoverable.basePacketID, recoverable.key, recoverable.sourceSpan, recoverable.missingIndex, err)
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
	packet, ipOK := recoveredIPv4Packet(reconstructed)
	if !ipOK {
		debuglog.Printf("recv", "recover_drop invalid_ipv4 packet_id=%d source_span=%d bytes=%d", packetID, recoverable.sourceSpan, len(reconstructed))
		return nil, false
	}
	window.addData(packetID, packet)
	if !state.dedupe.mark(packetID) {
		debuglog.Printf("recv", "recover_drop duplicate packet_id=%d", packetID)
		return nil, false
	}
	debuglog.Printf("recv", "recover_emit packet_id=%d source_span=%d bytes=%d", packetID, recoverable.sourceSpan, len(packet))
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "recover_emit"),
		metrics.L("session", sessionID),
		metrics.L("source_span", recoverable.sourceSpan),
	)
	pkt := packetbuf.Acquire(len(packet))
	copy(pkt.Payload, packet)
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
		if debuglog.Enabled() {
			debuglog.Printf("recv", "packet_emit bytes=%d zero_copy=true", len(payload))
		}
		return true, nil
	case <-ctx.Done():
		if debuglog.Enabled() {
			debuglog.Printf("recv", "packet_emit_drop ctx_done bytes=%d", len(payload))
		}
		return false, ctx.Err()
	}
}

func (o *Recv) recvState(sessionID uint64) *recvState {
	// Fast path: read-locked lookup. The Get/lookup pair stays inside
	// statesMu so a concurrent closeRecvState cannot tear the session down
	// between Get and the map lookup.
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

	// Slow path: install a fresh state under the write lock.
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
		rxWindows: make(map[uint8]*rxSLCWindow),
		dedupe:    newEmitDedupe(0),
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
