package send

import (
	"context"
	"errors"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
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
	debuglog.Printf("send", "tun_in bytes=%d", len(packet.Payload))
	return i.handleTUNPacket(ctx, packet.Payload)
}

func (i *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	debuglog.Printf("send", "transport_queue leg={%s} bytes=%d", debugLeg(leg), len(packet.Payload))
	select {
	case i.packets <- transport.Payload{Leg: leg, Packet: packet}:
		return nil
	case <-ctx.Done():
		packet.Release()
		debuglog.Printf("send", "transport_queue_drop ctx_done leg={%s}", debugLeg(leg))
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
		debuglog.Printf("send", "tun_drop no_active_session bytes=%d", len(packet))
		return nil
	}
	packetID, laneID, charge, err := l.writeTUNPacket(ctx, l.activeSessionID, packet)
	if errors.Is(err, errNoRunnableLane) {
		debuglog.Printf("send", "tun_drop no_runnable_lane session=%d packet_id=%d bytes=%d", l.activeSessionID, packetID, len(packet))
		return nil
	}
	debuglog.Printf("send", "tun_done session=%d packet_id=%d lane=%d charge=%d bytes=%d err=%v", l.activeSessionID, packetID, laneID, charge, len(packet), err)
	return err
}

func (l *Send) writeTUNPacket(ctx context.Context, sessionID uint64, packet []byte) (packetID uint32, laneID uint8, charge uint32, err error) {
	session := l.session(sessionID)
	packetID = session.nextPacketID
	debuglog.Printf("send", "data_schedule session=%d packet_id=%d bytes=%d fec_profile=%d", sessionID, packetID, len(packet), session.fecProfile)

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
			debuglog.Printf("send", "fec_group_ready session=%d base_packet_id=%d shards=%d", sessionID, group.basePacketID, len(group.packets))
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
			debuglog.Printf("send", "schedule_empty session=%d frame=%s", frame.SessionID, debugFrameSummary(frame))
			return 0, 0, errNoRunnableLane
		}

		lane := l.lanes[laneKey{sessionID: frame.SessionID, laneID: laneID}]
		if lane == nil {
			debuglog.Printf("send", "schedule_skip missing_lane session=%d lane=%d", frame.SessionID, laneID)
			continue
		}
		lane.queued = false
		if !lane.ready() {
			debuglog.Printf("send", "schedule_skip not_ready %s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane))
			continue
		}

		frame.LaneID = laneID
		leg, ok := lane.selectLeg()
		if !ok {
			debuglog.Printf("send", "schedule_skip no_leg %s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane))
			continue
		}
		debuglog.Printf("send", "schedule_select %s leg={%s} frame=%s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane), debugLeg(leg), debugFrameSummary(frame))
		size, err := l.enqueueFrame(ctx, leg, frame)
		if err != nil {
			debuglog.Printf("send", "schedule_enqueue_err session=%d lane=%d leg={%s} err=%v", frame.SessionID, laneID, debugLeg(leg), err)
			return laneID, 0, err
		}

		charge = legCharge(leg, size)
		if err := sched.Enqueue(laneID, lane.weight, charge); err != nil {
			debuglog.Printf("send", "schedule_requeue_err session=%d lane=%d charge=%d err=%v", frame.SessionID, laneID, charge, err)
			return laneID, charge, err
		}
		lane.queued = true
		debuglog.Printf("send", "schedule_done session=%d lane=%d leg={%s} frame_bytes=%d charge=%d", frame.SessionID, laneID, debugLeg(leg), size, charge)
		return laneID, charge, nil
	}
}

func (l *Send) maybeSendRepair(ctx context.Context, sessionID uint64, group txRepairGroup) {
	session := l.sessions[sessionID]
	if session == nil || session.fecCodec == nil {
		debuglog.Printf("send", "repair_skip session=%d session_nil=%t fec_nil=%t", sessionID, session == nil, session == nil || session.fecCodec == nil)
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
		debuglog.Printf("send", "repair_encode_err session=%d base_packet_id=%d key=%d err=%v", sessionID, group.basePacketID, key, err)
		return
	}
	debuglog.Printf("send", "repair_encode session=%d base_packet_id=%d key=%d symbol_len=%d", sessionID, group.basePacketID, key, len(shards[len(shards)-1]))

	laneID, charge, err := l.writeScheduledFrame(ctx, protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: sessionID,
		Body: protocol.RepairBody{
			BasePacketID: group.basePacketID,
			Key:          key,
			Symbol:       shards[len(shards)-1],
		},
	})
	debuglog.Printf("send", "repair_send session=%d base_packet_id=%d key=%d lane=%d charge=%d err=%v", sessionID, group.basePacketID, key, laneID, charge, err)
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
		debuglog.Printf("send", "start_lane_invalid session=%d lane=%d weight=%d", cfg.SessionID, cfg.LaneID, cfg.Weight)
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
	debuglog.Printf("send", "start_lane %s leg={%s} tcp_remote=%s nonce=%d caps=%#x fec_profile=%d", debugLaneState(key, lane), debugLeg(cfg.Leg), cfg.TCPRemote, cfg.Nonce, cfg.Caps, cfg.FECProfile)
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
		debuglog.Printf("send", "start_lane_write_err session=%d lane=%d leg={%s} err=%v", cfg.SessionID, cfg.LaneID, debugLeg(cfg.Leg), err)
		return err
	}
	lane.helloRetry.start(cfg.Nonce, cfg.Leg, retryPayload, cfg.Caps, cfg.FECProfile)
	debuglog.Printf("send", "start_lane_hello session=%d lane=%d nonce=%d bytes=%d", cfg.SessionID, cfg.LaneID, cfg.Nonce, len(retryPayload))
	return nil
}
