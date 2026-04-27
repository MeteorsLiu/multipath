package send

import (
	"context"
	"errors"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
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

func (i *Send) bootstrap(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bootstrapLocked(ctx)
}

func (i *Send) writeProbeEvent(ctx context.Context, event probe.Event) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.handleProbeEvent(ctx, event)
}

func (i *Send) retryHELLO(ctx context.Context, nowMS uint64) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.retryOpenHELLO(ctx, nowMS)
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
	_, state, ok := l.getOrCreateSessionState(sessionID)
	if !ok {
		return 0, 0, 0, errNoRunnableLane
	}
	packetID = state.nextPacketID
	debuglog.Printf("send", "data_schedule session=%d packet_id=%d bytes=%d fec_profile=%d", sessionID, packetID, len(packet), l.fecProfile)

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

	state.nextPacketID++
	if l.fecProfile == protocol.FECProfileSLC4Plus1 {
		if group, ok := state.txWindow.add(packetID, packet); ok {
			debuglog.Printf("send", "fec_group_ready session=%d base_packet_id=%d shards=%d", sessionID, group.basePacketID, len(group.packets))
			l.maybeSendRepair(ctx, sessionID, group)
		}
	}
	return packetID, laneID, charge, nil
}

func (l *Send) writeScheduledFrame(ctx context.Context, frame protocol.Frame) (laneID uint8, charge uint32, err error) {
	sizeHint, err := frameEncodeCapacity(frame)
	if err != nil {
		return 0, 0, err
	}
	lane, ok := l.pickLane(frame.SessionID, uint32(sizeHint))
	if !ok {
		debuglog.Printf("send", "schedule_empty session=%d frame=%s", frame.SessionID, debugFrameSummary(frame))
		metrics.IncCounter(metrics.ScheduleNoRunnableTotal,
			metrics.L("session", frame.SessionID),
			metrics.L("frame_type", debugFrameType(frame.Type)),
		)
		return 0, 0, errNoRunnableLane
	}

	laneID = lane.id
	frame.LaneID = laneID
	leg, ok := lane.selectLeg()
	if !ok {
		debuglog.Printf("send", "schedule_skip no_leg %s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane))
		metrics.IncCounter(metrics.ScheduleSkipTotal,
			metrics.L("session", frame.SessionID),
			metrics.L("lane", laneID),
			metrics.L("reason", "no_leg"),
		)
		return 0, 0, errNoRunnableLane
	}
	debuglog.Printf("send", "schedule_select %s leg={%s} frame=%s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane), debugLeg(leg), debugFrameSummary(frame))
	metrics.IncCounter(metrics.SchedulePickTotal,
		metrics.L("session", frame.SessionID),
		metrics.L("lane", laneID),
		metrics.L("frame_type", debugFrameType(frame.Type)),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	size, err := l.enqueueFrame(ctx, leg, frame)
	if err != nil {
		debuglog.Printf("send", "schedule_enqueue_err session=%d lane=%d leg={%s} err=%v", frame.SessionID, laneID, debugLeg(leg), err)
		return laneID, 0, err
	}

	charge = legCharge(leg, size)
	debuglog.Printf("send", "schedule_done session=%d lane=%d leg={%s} frame_bytes=%d charge=%d", frame.SessionID, laneID, debugLeg(leg), size, charge)
	return laneID, charge, nil
}

func (l *Send) maybeSendRepair(ctx context.Context, sessionID uint64, group txRepairGroup) {
	_, state, ok := l.getSessionState(sessionID)
	if !ok || l.fecCodec == nil {
		debuglog.Printf("send", "repair_skip session=%d session_nil=%t fec_nil=%t", sessionID, !ok, !ok || l.fecCodec == nil)
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
	key := state.nextRepairKey
	state.nextRepairKey++
	if err := l.fecCodec.Encode(shards, key); err != nil {
		debuglog.Printf("send", "repair_encode_err session=%d base_packet_id=%d key=%d err=%v", sessionID, group.basePacketID, key, err)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_encode_err"),
			metrics.L("session", sessionID),
		)
		return
	}
	debuglog.Printf("send", "repair_encode session=%d base_packet_id=%d key=%d symbol_len=%d", sessionID, group.basePacketID, key, len(shards[len(shards)-1]))
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_encode"),
		metrics.L("session", sessionID),
	)

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
	Caps       uint16
	FECProfile uint8
}

func (l *Send) startLane(ctx context.Context, cfg startLaneConfig) error {
	if cfg.Weight == 0 || cfg.LaneID == protocol.SessionControlLaneID {
		debuglog.Printf("send", "start_lane_invalid session=%d lane=%d weight=%d", cfg.SessionID, cfg.LaneID, cfg.Weight)
		return errInvalidLane
	}

	sessionState, _, ok := l.getOrCreateSessionState(cfg.SessionID)
	if !ok {
		return nil
	}
	l.activateSession(cfg.SessionID)
	key := laneKey{sessionID: cfg.SessionID, laneID: cfg.LaneID}
	lane := l.lanes[key]
	if lane == nil {
		lane = newLaneRuntime(cfg.LaneID, cfg.Weight)
		l.lanes[key] = lane
	}
	lane.weight = cfg.Weight
	lane.rememberLeg(cfg.Leg)
	l.markRunnableLanesDirty(cfg.SessionID)
	if cfg.TCPRemote != "" {
		lane.tcpRemote = cfg.TCPRemote
	}
	lane.helloCaps = cfg.Caps
	lane.helloFECProfile = cfg.FECProfile
	l.cancelHelloRoute(key)
	hello := sessionState.Open(0)
	var nonce uint64
	var retryPayload []byte
	var packet *packetbuf.Packet
	if err := hello.Do(func(v sessionpkg.View) error {
		nonce = v.Nonce()
		frame := protocol.Frame{
			Type:      protocol.TypeHELLO,
			SessionID: v.SessionID(),
			LaneID:    cfg.LaneID,
			Body: protocol.HelloBody{
				Nonce:      nonce,
				Caps:       cfg.Caps,
				FECProfile: cfg.FECProfile,
			},
		}
		var err error
		packet, err = l.encodePacket(frame)
		if err != nil {
			return err
		}
		retryPayload = append([]byte(nil), packet.Payload...)
		return nil
	}); err != nil {
		return err
	}
	debuglog.Printf("send", "start_lane %s leg={%s} tcp_remote=%s nonce=%d caps=%#x fec_profile=%d", debugLaneState(key, lane), debugLeg(cfg.Leg), cfg.TCPRemote, nonce, cfg.Caps, cfg.FECProfile)
	if err := l.WriteTo(ctx, cfg.Leg, packet); err != nil {
		debuglog.Printf("send", "start_lane_write_err session=%d lane=%d leg={%s} err=%v", cfg.SessionID, cfg.LaneID, debugLeg(cfg.Leg), err)
		return err
	}
	var route helloRoute
	route.set(hello, cfg.Leg, retryPayload)
	l.helloRoutes[key] = route
	debuglog.Printf("send", "start_lane_hello session=%d lane=%d nonce=%d bytes=%d", cfg.SessionID, cfg.LaneID, nonce, len(retryPayload))
	return nil
}
