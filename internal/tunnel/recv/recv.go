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

const defaultPacketQueueSize = 1024

type ControlState interface {
	OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
}

type Config struct {
	Control        ControlState
	SessionManager *sessionpkg.Manager
}

// Recv decodes transport-bound frames into IP packets and dispatches control
// frames to its ControlState.
//
// Locking convention:
//
//   - statesMu (RWMutex) protects the states map and is also held while
//     calling sessionManager.Get/Delete to keep the "session admitted iff
//     state present" invariant under concurrent close.
//   - recvState.mu protects the per-session receive window and closed flag.
//     It is taken AFTER releasing statesMu, never the other way around.
type Recv struct {
	statesMu sync.RWMutex
	control  ControlState
	manager  *sessionpkg.Manager
	fecCodec fecCodec
	packets  chan *packetbuf.Packet
	states   map[*sessionpkg.Session]*recvState
}

type recvState struct {
	mu       sync.Mutex
	closed   bool
	rxWindow *rxSLCWindow

	// shardScratch is reused by recoverPacket to materialize the shard
	// slice for fec.Reconstruct without per-call allocation. Size is fixed
	// at the maximum supported FEC group (sourceCount + 1 repair).
	shardScratch [8][]byte
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
	out.fecCodec, _ = fecpkg.NewCodec(4, 1)
	for _, cfg := range configs {
		if cfg.Control != nil {
			out.control = cfg.Control
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
	if o.control == nil {
		debuglog.Printf("recv/control", "hello_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.control.OnHello(ctx, leg, frame)
}

func (o *Recv) handleHELLOACK(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.HelloAckBody); !ok {
		debuglog.Printf("recv/control", "hello_ack_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "hello_ack session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.control == nil {
		debuglog.Printf("recv/control", "hello_ack_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.control.OnHelloAck(ctx, leg, frame)
}

func (o *Recv) handlePING(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.PingBody); !ok {
		debuglog.Printf("recv/control", "ping_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "ping session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.control == nil {
		debuglog.Printf("recv/control", "ping_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.control.OnPing(ctx, leg, frame)
}

func (o *Recv) handlePONG(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.PingBody); !ok {
		debuglog.Printf("recv/control", "pong_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "pong session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.control == nil {
		debuglog.Printf("recv/control", "pong_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.control.OnPong(ctx, leg, frame)
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
	recoverable, recoverableOK := state.rxWindow.addData(body.PacketID, body.Packet)
	emit := state.rxWindow.markEmitted(body.PacketID)
	state.mu.Unlock()
	if debuglog.Enabled() {
		debuglog.Printf("recv", "data_fec_window session=%d packet_id=%d recoverable=%t emit=%t", frame.SessionID, body.PacketID, recoverableOK, emit)
	}
	if !emit {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop_duplicate session=%d packet_id=%d", frame.SessionID, body.PacketID)
		}
		return false, nil
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
		return consumed, o.maybeRecover(ctx, frame.SessionID, state, recoverable)
	}
	return consumed, nil
}

func (o *Recv) handleREPAIR(ctx context.Context, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		debuglog.Printf("recv", "repair_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "repair_accept session=%d lane=%d base_packet_id=%d key=%d symbol_len=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, len(body.Symbol))
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_accept"),
		metrics.L("session", frame.SessionID),
	)

	state := o.recvState(frame.SessionID)
	if state == nil {
		debuglog.Printf("recv", "repair_drop missing_session session=%d lane=%d base_packet_id=%d key=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key)
		return nil
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		debuglog.Printf("recv", "repair_drop closed_session session=%d lane=%d base_packet_id=%d key=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key)
		return nil
	}
	if recoverable, ok := state.rxWindow.addRepair(body.BasePacketID, body.Key, body.Symbol); ok {
		state.mu.Unlock()
		debuglog.Printf("recv", "repair_recoverable session=%d base_packet_id=%d key=%d missing_index=%d", frame.SessionID, recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_recoverable"),
			metrics.L("session", frame.SessionID),
		)
		return o.maybeRecover(ctx, frame.SessionID, state, recoverable)
	}
	state.mu.Unlock()
	debuglog.Printf("recv", "repair_stored session=%d base_packet_id=%d key=%d", frame.SessionID, body.BasePacketID, body.Key)
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
	if o.control == nil {
		debuglog.Printf("recv/control", "close_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
	} else if err := o.control.OnClose(ctx, leg, frame); err != nil {
		return err
	}
	if body.Scope == protocol.CloseScopeSession {
		o.closeRecvState(frame.SessionID, session)
		debuglog.Printf("recv", "close_session session=%d", frame.SessionID)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, sessionID uint64, state *recvState, recoverable rxRecoverable) error {
	if state == nil || o.fecCodec == nil {
		debuglog.Printf("recv", "recover_skip session_nil=%t fec_nil=%t base_packet_id=%d key=%d missing_index=%d", state == nil, o.fecCodec == nil, recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		return nil
	}
	packet, ok := o.recoverPacket(sessionID, state, recoverable)
	if !ok {
		return nil
	}
	return o.emitCopiedPacket(ctx, packet)
}

func (o *Recv) recoverPacket(sessionID uint64, state *recvState, recoverable rxRecoverable) ([]byte, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		debuglog.Printf("recv", "recover_drop closed_session base_packet_id=%d key=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		return nil, false
	}
	shards, ok := state.rxWindow.buildShardsLocked(recoverable, state.shardScratch[:0])
	if !ok {
		// Window state changed (e.g. concurrent close/prune); recovery no
		// longer applicable.
		debuglog.Printf("recv", "recover_drop window_stale base_packet_id=%d key=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		return nil, false
	}
	if err := o.fecCodec.Reconstruct(shards, recoverable.key); err != nil {
		debuglog.Printf("recv", "recover_err base_packet_id=%d key=%d missing_index=%d err=%v", recoverable.basePacketID, recoverable.key, recoverable.missingIndex, err)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "recover_err"),
			metrics.L("session", sessionID),
		)
		return nil, false
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "reconstruct_done"),
		metrics.L("session", sessionID),
	)

	packetID := recoverable.basePacketID + uint32(recoverable.missingIndex)
	reconstructed := shards[recoverable.missingIndex]
	packet, ipOK := recoveredIPv4Packet(reconstructed)
	if !ipOK {
		debuglog.Printf("recv", "recover_drop invalid_ipv4 packet_id=%d bytes=%d", packetID, len(reconstructed))
		return nil, false
	}
	state.rxWindow.addData(packetID, packet)
	if !state.rxWindow.markEmitted(packetID) {
		debuglog.Printf("recv", "recover_drop duplicate packet_id=%d", packetID)
		return nil, false
	}
	debuglog.Printf("recv", "recover_emit packet_id=%d bytes=%d", packetID, len(packet))
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "recover_emit"),
		metrics.L("session", sessionID),
	)
	return append([]byte(nil), packet...), true
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

func (o *Recv) emitCopiedPacket(ctx context.Context, payload []byte) error {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	select {
	case o.packets <- packet:
		if debuglog.Enabled() {
			debuglog.Printf("recv", "packet_emit bytes=%d zero_copy=false", len(payload))
		}
		return nil
	case <-ctx.Done():
		packet.Release()
		if debuglog.Enabled() {
			debuglog.Printf("recv", "packet_emit_drop ctx_done bytes=%d zero_copy=false", len(payload))
		}
		return ctx.Err()
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
		rxWindow: newRxSLCWindow(4),
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
	state.rxWindow.releaseAll()
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
