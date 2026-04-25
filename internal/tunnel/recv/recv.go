package recv

import (
	"context"
	"sync"

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
		return nil
	}

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
	if err != nil {
		return err
	}
	if result.Accepted {
		o.session(frame.SessionID).fecProfile = result.FECProfile
	}
	return nil
}

func (o *Recv) handleHELLOACK(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	result, err := o.controller.Write(ctx, frame, leg)
	if err != nil {
		return err
	}
	if result.Accepted {
		o.session(frame.SessionID).fecProfile = result.FECProfile
	}
	return nil
}

func (o *Recv) handlePING(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	_, err := o.controller.Write(ctx, frame, leg)
	return err
}

func (o *Recv) handlePONG(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	_, err := o.controller.Write(ctx, frame, leg)
	return err
}

func (o *Recv) handleDATA(ctx context.Context, leg transport.LegRef, frame protocol.Frame, packet *packetbuf.Packet) (bool, error) {
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		return false, protocol.ErrInvalidFrame
	}
	result, err := o.controller.Write(ctx, frame, leg)
	if err != nil || !result.Accepted {
		return false, err
	}

	session := o.session(frame.SessionID)
	var recoverable rxRecoverable
	recoverableOK := false
	emit := true
	if session.fecProfile == protocol.FECProfileSLC4Plus1 {
		recoverable, recoverableOK = session.rxWindow.addData(body.PacketID, body.Packet)
		emit = session.rxWindow.markEmitted(body.PacketID)
	}
	if !emit {
		return false, nil
	}
	consumed, err := o.emitTransportPacket(ctx, packet, body.Packet)
	if err != nil {
		return false, err
	}
	if recoverableOK {
		return consumed, o.maybeRecover(ctx, session, recoverable)
	}
	return consumed, nil
}

func (o *Recv) handleREPAIR(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	result, err := o.controller.Write(ctx, frame, leg)
	if err != nil || !result.Accepted {
		return err
	}

	session := o.sessions[frame.SessionID]
	if session == nil || session.fecProfile != protocol.FECProfileSLC4Plus1 {
		return nil
	}
	if recoverable, ok := session.rxWindow.addRepair(body.BasePacketID, body.Key, body.Symbol); ok {
		return o.maybeRecover(ctx, session, recoverable)
	}
	return nil
}

func (o *Recv) handleCLOSE(ctx context.Context, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	if _, err := o.controller.Write(ctx, frame, transport.LegRef{}); err != nil {
		return err
	}
	if body.Scope == protocol.CloseScopeSession {
		delete(o.sessions, frame.SessionID)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, session *rxSession, recoverable rxRecoverable) error {
	if session == nil || session.fecCodec == nil {
		return nil
	}
	if err := session.fecCodec.Reconstruct(recoverable.shards, recoverable.key); err != nil {
		return nil
	}

	packetID := recoverable.basePacketID + uint32(recoverable.missingIndex)
	packet := recoverable.shards[recoverable.missingIndex]
	var ok bool
	packet, ok = recoveredIPv4Packet(packet)
	if !ok {
		return nil
	}
	session.rxWindow.addData(packetID, packet)
	if !session.rxWindow.markEmitted(packetID) {
		return nil
	}
	return o.emitCopiedPacket(ctx, packet)
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

func (o *Recv) emitCopiedPacket(ctx context.Context, payload []byte) error {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	select {
	case o.packets <- packet:
		return nil
	case <-ctx.Done():
		packet.Release()
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
