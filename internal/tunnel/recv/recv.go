package recv

import (
	"context"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const defaultPacketQueueSize = 1024

type Config struct {
	Controller interface {
		Write(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (Result, error)
	}
}

type controller interface {
	Write(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (Result, error)
}

type Result struct {
	Accepted   bool
	Caps       uint16
	FECProfile uint8
}

type Recv struct {
	mu         sync.Mutex
	controller controller
	packets    chan *packetbuf.Packet
	sessions   map[uint64]*rxSession
}

type rxSession struct {
	fecProfile uint8
	fecCodec   fecCodec
	rxWindow   *rxSLCWindow
}

type fecCodec interface {
	Reconstruct(shards [][]byte, key uint16) error
}

func New(configs ...Config) *Recv {
	out := &Recv{
		sessions: make(map[uint64]*rxSession),
		packets:  make(chan *packetbuf.Packet, defaultPacketQueueSize),
	}
	for _, cfg := range configs {
		out.applyConfig(cfg)
	}
	return out
}

func (o *Recv) Packets() <-chan *packetbuf.Packet {
	return o.packets
}

func (o *Recv) applyConfig(cfg Config) {
	if cfg.Controller != nil {
		o.controller = cfg.Controller
	}
}

func (o *Recv) Write(ctx context.Context, packet *packetbuf.Packet) error {
	return o.WriteTo(ctx, transport.LegRef{}, packet)
}

func (o *Recv) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error {
	o.mu.Lock()
	defer o.mu.Unlock()

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
		debuglog.Printf("recv", "decode_drop leg={%s} bytes=%d err=%v", debugLeg(leg), len(packet.Payload), err)
		return nil
	}
	debuglog.Printf("recv", "frame_in %s leg={%s}", debugFrameSummary(frame), debugLeg(leg))

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
		consumed, err := o.handleDATA(ctx, leg, frame, packet)
		if consumed {
			releaseEventPacket = false
		}
		return err
	case protocol.TypeREPAIR:
		return o.handleREPAIR(ctx, leg, frame)
	case protocol.TypeCLOSE:
		return o.handleCLOSE(ctx, frame)
	default:
		return nil
	}
}

func (o *Recv) handleHELLO(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	result, err := o.controller.Write(ctx, frame, leg)
	debuglog.Printf("recv", "hello_controller session=%d lane=%d accepted=%t caps=%#x fec_profile=%d err=%v", frame.SessionID, frame.LaneID, result.Accepted, result.Caps, result.FECProfile, err)
	if err != nil {
		return err
	}
	if result.Accepted {
		o.session(frame.SessionID).fecProfile = result.FECProfile
		debuglog.Printf("recv", "hello_accept session=%d lane=%d fec_profile=%d", frame.SessionID, frame.LaneID, result.FECProfile)
	}
	return nil
}

func (o *Recv) handleHELLOACK(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	result, err := o.controller.Write(ctx, frame, leg)
	debuglog.Printf("recv", "hello_ack_controller session=%d lane=%d accepted=%t caps=%#x fec_profile=%d err=%v", frame.SessionID, frame.LaneID, result.Accepted, result.Caps, result.FECProfile, err)
	if err != nil {
		return err
	}
	if result.Accepted {
		o.session(frame.SessionID).fecProfile = result.FECProfile
		debuglog.Printf("recv", "hello_ack_accept session=%d lane=%d fec_profile=%d", frame.SessionID, frame.LaneID, result.FECProfile)
	}
	return nil
}

func (o *Recv) handlePING(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	_, err := o.controller.Write(ctx, frame, leg)
	debuglog.Printf("recv", "ping_controller session=%d lane=%d err=%v", frame.SessionID, frame.LaneID, err)
	return err
}

func (o *Recv) handlePONG(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	_, err := o.controller.Write(ctx, frame, leg)
	debuglog.Printf("recv", "pong_controller session=%d lane=%d err=%v", frame.SessionID, frame.LaneID, err)
	return err
}

func (o *Recv) handleDATA(ctx context.Context, leg transport.LegRef, frame protocol.Frame, packet *packetbuf.Packet) (bool, error) {
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		debuglog.Printf("recv", "data_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return false, protocol.ErrInvalidFrame
	}
	result, err := o.controller.Write(ctx, frame, leg)
	if err != nil || !result.Accepted {
		debuglog.Printf("recv", "data_controller_drop session=%d lane=%d packet_id=%d accepted=%t err=%v", frame.SessionID, frame.LaneID, body.PacketID, result.Accepted, err)
		return false, err
	}

	session := o.session(frame.SessionID)
	debuglog.Printf("recv", "data_accept session=%d lane=%d packet_id=%d bytes=%d fec_profile=%d", frame.SessionID, frame.LaneID, body.PacketID, len(body.Packet), session.fecProfile)
	var recoverable rxRecoverable
	recoverableOK := false
	emit := true
	if session.fecProfile == protocol.FECProfileSLC4Plus1 {
		recoverable, recoverableOK = session.rxWindow.addData(body.PacketID, body.Packet)
		emit = session.rxWindow.markEmitted(body.PacketID)
		debuglog.Printf("recv", "data_fec_window session=%d packet_id=%d recoverable=%t emit=%t", frame.SessionID, body.PacketID, recoverableOK, emit)
	}
	if !emit {
		debuglog.Printf("recv", "data_drop_duplicate session=%d packet_id=%d", frame.SessionID, body.PacketID)
		return false, nil
	}
	consumed, err := o.emitTransportPacket(ctx, packet, body.Packet)
	if err != nil {
		debuglog.Printf("recv", "data_emit_err session=%d packet_id=%d err=%v", frame.SessionID, body.PacketID, err)
		return false, err
	}
	debuglog.Printf("recv", "data_emit session=%d packet_id=%d consumed=%t bytes=%d", frame.SessionID, body.PacketID, consumed, len(body.Packet))
	if recoverableOK {
		return consumed, o.maybeRecover(ctx, session, recoverable)
	}
	return consumed, nil
}

func (o *Recv) handleREPAIR(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		debuglog.Printf("recv", "repair_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	result, err := o.controller.Write(ctx, frame, leg)
	if err != nil || !result.Accepted {
		debuglog.Printf("recv", "repair_controller_drop session=%d lane=%d base_packet_id=%d key=%d accepted=%t err=%v", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, result.Accepted, err)
		return err
	}
	debuglog.Printf("recv", "repair_accept session=%d lane=%d base_packet_id=%d key=%d symbol_len=%d", frame.SessionID, frame.LaneID, body.BasePacketID, body.Key, len(body.Symbol))

	session := o.sessions[frame.SessionID]
	if session == nil || session.fecProfile != protocol.FECProfileSLC4Plus1 {
		debuglog.Printf("recv", "repair_skip session=%d session_nil=%t fec_profile=%d", frame.SessionID, session == nil, func() uint8 {
			if session == nil {
				return 0
			}
			return session.fecProfile
		}())
		return nil
	}
	if recoverable, ok := session.rxWindow.addRepair(body.BasePacketID, body.Key, body.Symbol); ok {
		debuglog.Printf("recv", "repair_recoverable session=%d base_packet_id=%d key=%d missing_index=%d", frame.SessionID, recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		return o.maybeRecover(ctx, session, recoverable)
	}
	debuglog.Printf("recv", "repair_stored session=%d base_packet_id=%d key=%d", frame.SessionID, body.BasePacketID, body.Key)
	return nil
}

func (o *Recv) handleCLOSE(ctx context.Context, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		debuglog.Printf("recv", "close_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	if _, err := o.controller.Write(ctx, frame, transport.LegRef{}); err != nil {
		debuglog.Printf("recv", "close_controller_err session=%d lane=%d scope=%d err=%v", frame.SessionID, frame.LaneID, body.Scope, err)
		return err
	}
	if body.Scope == protocol.CloseScopeSession {
		delete(o.sessions, frame.SessionID)
		debuglog.Printf("recv", "close_session session=%d", frame.SessionID)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, session *rxSession, recoverable rxRecoverable) error {
	if session == nil || session.fecCodec == nil {
		debuglog.Printf("recv", "recover_skip session_nil=%t fec_nil=%t base_packet_id=%d key=%d missing_index=%d", session == nil, session == nil || session.fecCodec == nil, recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
		return nil
	}
	if err := session.fecCodec.Reconstruct(recoverable.shards, recoverable.key); err != nil {
		debuglog.Printf("recv", "recover_err base_packet_id=%d key=%d missing_index=%d err=%v", recoverable.basePacketID, recoverable.key, recoverable.missingIndex, err)
		return nil
	}

	packetID := recoverable.basePacketID + uint32(recoverable.missingIndex)
	packet := recoverable.shards[recoverable.missingIndex]
	var ok bool
	packet, ok = recoveredIPv4Packet(packet)
	if !ok {
		debuglog.Printf("recv", "recover_drop invalid_ipv4 packet_id=%d bytes=%d", packetID, len(recoverable.shards[recoverable.missingIndex]))
		return nil
	}
	session.rxWindow.addData(packetID, packet)
	if !session.rxWindow.markEmitted(packetID) {
		debuglog.Printf("recv", "recover_drop duplicate packet_id=%d", packetID)
		return nil
	}
	debuglog.Printf("recv", "recover_emit packet_id=%d bytes=%d", packetID, len(packet))
	return o.emitCopiedPacket(ctx, packet)
}

func (o *Recv) emitTransportPacket(ctx context.Context, packet *packetbuf.Packet, payload []byte) (bool, error) {
	packet.Payload = payload
	select {
	case o.packets <- packet:
		debuglog.Printf("recv", "packet_emit bytes=%d zero_copy=true", len(payload))
		return true, nil
	case <-ctx.Done():
		debuglog.Printf("recv", "packet_emit_drop ctx_done bytes=%d", len(payload))
		return false, ctx.Err()
	}
}

func (o *Recv) emitCopiedPacket(ctx context.Context, payload []byte) error {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	select {
	case o.packets <- packet:
		debuglog.Printf("recv", "packet_emit bytes=%d zero_copy=false", len(payload))
		return nil
	case <-ctx.Done():
		packet.Release()
		debuglog.Printf("recv", "packet_emit_drop ctx_done bytes=%d zero_copy=false", len(payload))
		return ctx.Err()
	}
}

func (o *Recv) session(sessionID uint64) *rxSession {
	session := o.sessions[sessionID]
	if session != nil {
		return session
	}
	session = &rxSession{
		rxWindow: newRxSLCWindow(4),
	}
	session.fecCodec, _ = fecpkg.NewCodec(4, 1)
	o.sessions[sessionID] = session
	debuglog.Printf("recv", "session_create session=%d fec_codec=%t", sessionID, session.fecCodec != nil)
	return session
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
