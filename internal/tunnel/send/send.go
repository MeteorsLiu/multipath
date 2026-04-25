package send

import (
	"context"
	"errors"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func (i *Send) Write(ctx context.Context, packet *packetbuf.Packet) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if packet == nil {
		return nil
	}
	defer packet.Release()
	return i.handleTUNPacket(ctx, packet.Payload)
}

func (i *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	select {
	case i.packets <- transport.Payload{Leg: leg, Packet: packet}:
		return nil
	case <-ctx.Done():
		packet.Release()
		return ctx.Err()
	}
}

func (i *Send) Bootstrap(ctx context.Context) error {
	return i.bootstrap(ctx)
}

func (i *Send) WriteFrame(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (Result, error) {
	return i.writeControlFrame(ctx, leg, frame)
}

func (i *Send) WriteProbeEvent(ctx context.Context, event probe.Event) error {
	return i.writeProbeEvent(ctx, event)
}

func (i *Send) RetryHELLO(ctx context.Context, nowMS uint64) error {
	return i.retryHELLO(ctx, nowMS)
}

func (i *Send) bootstrap(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bootstrapLocked(ctx)
}

func (i *Send) writeControlFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (Result, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.handleTransportFrame(ctx, leg, frame)
}

func (i *Send) writeProbeEvent(ctx context.Context, event probe.Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.handleProbeEvent(ctx, event)
}

func (i *Send) retryHELLO(ctx context.Context, nowMS uint64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.retryPendingHELLO(ctx, nowMS)
}

func (i *Send) finishFallbackDial(ctx context.Context, result fallbackDialResult) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.handleFallbackDialResult(ctx, result)
}

func (l *Send) handleTUNPacket(ctx context.Context, packet []byte) error {
	if !l.hasActiveSession {
		return nil
	}
	_, _, _, err := l.writeTUNPacket(ctx, l.activeSessionID, packet)
	if errors.Is(err, errNoRunnableLane) {
		return nil
	}
	return err
}

func (l *Send) writeTUNPacket(ctx context.Context, sessionID uint64, packet []byte) (packetID uint32, laneID uint8, charge uint32, err error) {
	session := l.session(sessionID)
	packetID = session.nextPacketID

	laneID, charge, err = l.writeScheduledFrame(ctx, protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: sessionID,
		Body: protocol.DataBody{
			PacketID: packetID,
			Packet:   packet,
		},
	})
	if err != nil {
		return packetID, laneID, charge, err
	}

	session.nextPacketID++
	if session.fecProfile == protocol.FECProfileSLC4Plus1 {
		if group, ok := session.txWindow.add(packetID, packet); ok {
			l.maybeSendRepair(ctx, sessionID, group)
		}
	}
	return packetID, laneID, charge, nil
}

func (l *Send) writeScheduledFrame(ctx context.Context, frame protocol.Frame) (laneID uint8, charge uint32, err error) {
	sched := l.scheduler(frame.SessionID)
	for {
		laneID, ok := sched.Dequeue()
		if !ok {
			return 0, 0, errNoRunnableLane
		}

		lane := l.lanes[laneKey{sessionID: frame.SessionID, laneID: laneID}]
		if lane == nil {
			continue
		}
		lane.queued = false
		if !lane.ready() {
			continue
		}

		frame.LaneID = laneID
		leg, ok := lane.selectLeg()
		if !ok {
			continue
		}
		size, err := l.enqueueFrame(ctx, leg, frame)
		if err != nil {
			return laneID, 0, err
		}

		charge = legCharge(leg, size)
		if err := sched.Enqueue(laneID, lane.weight, charge); err != nil {
			return laneID, charge, err
		}
		lane.queued = true
		return laneID, charge, nil
	}
}

func (l *Send) maybeSendRepair(ctx context.Context, sessionID uint64, group txRepairGroup) {
	session := l.sessions[sessionID]
	if session == nil || session.fecCodec == nil {
		return
	}

	var shardBuf [5][]byte
	var shards [][]byte
	if len(group.packets)+1 > len(shardBuf) {
		shards = make([][]byte, len(group.packets)+1)
	} else {
		shards = shardBuf[:len(group.packets)+1]
	}
	for i := range group.packets {
		shards[i] = group.packets[i]
	}
	key := session.nextRepairKey
	session.nextRepairKey++
	if err := session.fecCodec.Encode(shards, key); err != nil {
		return
	}

	_, _, _ = l.writeScheduledFrame(ctx, protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: sessionID,
		Body: protocol.RepairBody{
			BasePacketID: group.basePacketID,
			Key:          key,
			Symbol:       shards[len(shards)-1],
		},
	})
}

type startLaneConfig struct {
	SessionID  uint64
	LaneID     uint8
	Weight     uint32
	Leg        transport.LegRef
	TCPRemote  string
	Nonce      uint64
	Caps       uint16
	FECProfile uint8
}

func (l *Send) startLane(ctx context.Context, cfg startLaneConfig) error {
	if cfg.Weight == 0 || cfg.LaneID == protocol.SessionControlLaneID {
		return errInvalidLane
	}

	l.session(cfg.SessionID)
	l.activateSession(cfg.SessionID)
	key := laneKey{sessionID: cfg.SessionID, laneID: cfg.LaneID}
	lane := l.lanes[key]
	if lane == nil {
		lane = newLaneRuntime(cfg.LaneID, cfg.Weight)
		l.lanes[key] = lane
	}
	lane.weight = cfg.Weight
	lane.rememberLeg(cfg.Leg)
	if cfg.TCPRemote != "" {
		lane.tcpRemote = cfg.TCPRemote
	}
	hello := protocol.Frame{
		Type:      protocol.TypeHELLO,
		SessionID: cfg.SessionID,
		LaneID:    cfg.LaneID,
		Body: protocol.HelloBody{
			Nonce:      cfg.Nonce,
			Caps:       cfg.Caps,
			FECProfile: cfg.FECProfile,
		},
	}
	packet, err := l.encodePacket(hello)
	if err != nil {
		return err
	}
	retryPayload := append([]byte(nil), packet.Payload...)
	if err := l.WriteTo(ctx, cfg.Leg, packet); err != nil {
		return err
	}
	lane.helloRetry.start(cfg.Nonce, cfg.Leg, retryPayload, cfg.Caps, cfg.FECProfile)
	return nil
}
