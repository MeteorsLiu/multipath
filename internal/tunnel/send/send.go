package send

import (
	"context"
	"errors"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

// Write enqueues an inbound TUN packet for transmission. It owns the packet
// and releases it before returning. Concurrent calls are safe; per-session
// and per-lane state is guarded by their own locks.
func (i *Send) Write(ctx context.Context, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	defer packet.Release()
	if debuglog.Enabled() {
		debuglog.Printf("send", "tun_in bytes=%d", len(packet.Payload))
	}
	return i.handleTUNPacket(ctx, packet.Payload)
}

// WriteTo enqueues an already-encoded transport payload onto the outbound
// queue. It must not be called while holding any Send lock; the channel send
// is the only blocking point in the send pipeline.
func (i *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("send", "transport_queue leg={%s} bytes=%d", debugLeg(leg), len(packet.Payload))
	}
	select {
	case i.packets <- transport.Payload{Leg: leg, Packet: packet}:
		return nil
	case <-ctx.Done():
		packet.Release()
		if debuglog.Enabled() {
			debuglog.Printf("send", "transport_queue_drop ctx_done leg={%s}", debugLeg(leg))
		}
		return ctx.Err()
	}
}

func (l *Send) handleTUNPacket(ctx context.Context, packet []byte) error {
	sessionID, ok := l.activeSession()
	if !ok {
		if debuglog.Enabled() {
			debuglog.Printf("send", "tun_drop no_active_session bytes=%d", len(packet))
		}
		return nil
	}
	packetID, laneID, charge, err := l.writeTUNPacket(ctx, sessionID, packet)
	if errors.Is(err, errNoRunnableLane) {
		if debuglog.Enabled() {
			debuglog.Printf("send", "tun_drop no_runnable_lane session=%d packet_id=%d bytes=%d", sessionID, packetID, len(packet))
		}
		return nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("send", "tun_done session=%d packet_id=%d lane=%d charge=%d bytes=%d err=%v", sessionID, packetID, laneID, charge, len(packet), err)
	}
	return err
}

func (l *Send) writeTUNPacket(ctx context.Context, sessionID uint64, packet []byte) (packetID uint32, laneID uint8, charge uint32, err error) {
	state := l.activeSendState.Load()
	if state == nil {
		// Cache miss: fall back to the locked path and prime the cache so
		// subsequent packets are lock-free.
		_, fresh, ok := l.getOrCreateSessionState(sessionID)
		if !ok {
			return 0, 0, 0, errNoRunnableLane
		}
		state = fresh
		l.activeSendState.Store(state)
	}
	packetID = state.reservePacketID()
	fecProfile := uint8(l.fecProfile.Load())
	if debuglog.Enabled() {
		debuglog.Printf("send", "data_schedule session=%d packet_id=%d bytes=%d fec_profile=%d", sessionID, packetID, len(packet), fecProfile)
	}

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

	if fecProfileEnabled(fecProfile) {
		lane := l.getLane(laneKey{sessionID: sessionID, laneID: laneID})
		if lane != nil {
			group, ready, armFlush := lane.commitPacket(packetID, packet)
			if ready {
				if debuglog.Enabled() {
					debuglog.Printf("send", "fec_group_ready session=%d lane=%d base_packet_id=%d source_span=%d shards=%d", sessionID, laneID, group.basePacketID, group.sourceSpan, len(group.packets))
				}
				l.maybeSendRepair(ctx, sessionID, lane, group)
			} else if armFlush && fecProfile == protocol.FECProfileSLCVariablePlus1 {
				l.armFECFlushTimer(sessionID, lane)
			}
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
		if debuglog.Enabled() {
			debuglog.Printf("send", "schedule_empty session=%d frame=%s", frame.SessionID, debugFrameSummary(frame))
		}
		metrics.IncCounter(metrics.ScheduleNoRunnableTotal,
			metrics.L("session", frame.SessionID),
			metrics.L("frame_type", debugFrameType(frame.Type)),
		)
		return 0, 0, errNoRunnableLane
	}
	charge, err = l.sendFrameOnLane(ctx, lane, frame, sizeHint)
	return lane.id, charge, err
}

// sendFrameOnLane encodes frame and enqueues it on the given lane, choosing the
// lane's transport leg via the leg selector. It does NOT run
// schedule.Strategy lane selection, so it is the path REPAIR and other
// already-routed frames take. It sets frame.LaneID to the lane id.
func (l *Send) sendFrameOnLane(ctx context.Context, lane *laneRuntime, frame protocol.Frame, sizeHint int) (uint32, error) {
	frame.LaneID = lane.id

	udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
	useUDP, ok := l.legSelector(frame.SessionID).Pick(udpQ, tcpQ)
	if !ok {
		if debuglog.Enabled() {
			debuglog.Printf("send", "schedule_skip no_leg %s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: lane.id}, lane))
		}
		metrics.IncCounter(metrics.ScheduleSkipTotal,
			metrics.L("session", frame.SessionID),
			metrics.L("lane", lane.id),
			metrics.L("reason", "no_leg"),
		)
		return 0, errNoRunnableLane
	}

	var leg transport.LegRef
	if useUDP {
		leg = udpLeg
	} else {
		leg = tcpLeg
	}
	if debuglog.Enabled() {
		debuglog.Printf("send", "schedule_select %s leg={%s} frame=%s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: lane.id}, lane), debugLeg(leg), debugFrameSummary(frame))
	}
	metrics.IncCounter(metrics.SchedulePickTotal,
		metrics.LU64("session", frame.SessionID),
		metrics.LU8("lane", lane.id),
		metrics.LStr("frame_type", debugFrameType(frame.Type)),
		metrics.LStr("leg", kindMetricLabel(leg.Kind)),
	)
	size, err := l.enqueueFrameWithSize(ctx, leg, frame, sizeHint)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "schedule_enqueue_err session=%d lane=%d leg={%s} err=%v", frame.SessionID, lane.id, debugLeg(leg), err)
		}
		return 0, err
	}

	charge := legCharge(leg, size)
	lane.quality.OnSent(leg.Kind, charge, time.Now())
	if debuglog.Enabled() {
		debuglog.Printf("send", "schedule_done session=%d lane=%d leg={%s} frame_bytes=%d charge=%d", frame.SessionID, lane.id, debugLeg(leg), size, charge)
	}
	return charge, nil
}

func (l *Send) maybeSendRepair(ctx context.Context, sessionID uint64, lane *laneRuntime, group txRepairGroup) {
	defer func() {
		for _, pkt := range group.packets {
			pkt.Release()
		}
	}()
	if lane == nil {
		return
	}

	_, state, ok := l.getSessionState(sessionID)
	sourceSpan := len(group.packets)
	codec := l.fecCodecForSourceSpan(sourceSpan)
	if !ok || codec == nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "repair_skip session=%d session_nil=%t fec_nil=%t source_span=%d", sessionID, !ok, codec == nil, sourceSpan)
		}
		return
	}

	var shardBuf [maxFECSourceSpan + 1][]byte
	var shards [][]byte
	if len(group.packets)+1 > len(shardBuf) {
		shards = make([][]byte, len(group.packets)+1)
	} else {
		shards = shardBuf[:len(group.packets)+1]
	}
	repairLen := 0
	for i := range group.packets {
		shards[i] = group.packets[i].Payload
		if n := len(shards[i]); n > repairLen {
			repairLen = n
		}
	}
	// Pre-size the repair shard with a pooled buffer so fec.Encode reuses
	// existing capacity instead of allocating a fresh ~1500B slice per group.
	repairPkt := packetbuf.Acquire(repairLen)
	defer repairPkt.Release()
	shards[len(shards)-1] = repairPkt.Payload[:repairLen]
	key := state.reserveRepairKey()
	if err := codec.Encode(shards, key); err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "repair_encode_err session=%d base_packet_id=%d key=%d source_span=%d err=%v", sessionID, group.basePacketID, key, sourceSpan, err)
		}
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_encode_err"),
			metrics.L("session", sessionID),
			metrics.L("source_span", sourceSpan),
		)
		return
	}
	if debuglog.Enabled() {
		debuglog.Printf("send", "repair_encode session=%d base_packet_id=%d key=%d source_span=%d symbol_len=%d", sessionID, group.basePacketID, key, sourceSpan, len(shards[len(shards)-1]))
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_encode"),
		metrics.L("session", sessionID),
		metrics.L("source_span", sourceSpan),
	)

	repairFrame := protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: sessionID,
		Body: protocol.RepairBody{
			BasePacketID: group.basePacketID,
			Key:          key,
			SourceSpan:   uint8(sourceSpan),
			Symbol:       shards[len(shards)-1],
		},
	}
	sizeHint, err := frameEncodeCapacity(repairFrame)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "repair_size_err session=%d base_packet_id=%d err=%v", sessionID, group.basePacketID, err)
		}
		return
	}
	charge, err := l.sendFrameOnLane(ctx, lane, repairFrame, sizeHint)
	if debuglog.Enabled() {
		debuglog.Printf("send", "repair_send session=%d base_packet_id=%d key=%d source_span=%d lane=%d charge=%d err=%v", sessionID, group.basePacketID, key, sourceSpan, lane.id, charge, err)
	}
}

type startLaneConfig struct {
	Session    *sessionpkg.Session
	LaneID     uint8
	Weight     uint32
	Leg        transport.LegRef
	TCPRemote  string
	Caps       uint16
	FECProfile uint8
}

func (l *Send) startLane(ctx context.Context, cfg startLaneConfig) error {
	sessionID, ok := sessionIDOf(cfg.Session)
	if !ok {
		return errSessionIDConflict
	}
	if cfg.Weight == 0 || cfg.LaneID == protocol.SessionControlLaneID {
		debuglog.Printf("send", "start_lane_invalid session=%d lane=%d weight=%d", sessionID, cfg.LaneID, cfg.Weight)
		return errInvalidLane
	}

	_, ok = l.getOrCreateSendState(cfg.Session)
	if !ok {
		return nil
	}
	l.activateSession(sessionID)

	key := laneKey{sessionID: sessionID, laneID: cfg.LaneID}
	lane := l.getOrCreateLane(key, cfg.Weight)
	lane.setWeight(cfg.Weight)
	lane.rememberLeg(cfg.Leg)
	if cfg.TCPRemote != "" {
		lane.setTCPRemote(cfg.TCPRemote)
	}
	lane.setHelloProfile(cfg.Caps, cfg.FECProfile)
	l.markRunnableLanesDirty(sessionID)
	l.cancelHelloRoute(key)

	hello := cfg.Session.Open(0)
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

	// Install the route before the channel send so observers that synchronize
	// on the outbound channel see a coherent map state. If WriteTo fails the
	// retry tick will re-send the same payload using the route.
	var route helloRoute
	route.set(hello, cfg.Leg, retryPayload, l.helloTimeoutForRoute(sessionID, lane, cfg.Leg))
	l.helloRoutesMu.Lock()
	l.helloRoutes[key] = route
	l.helloRoutesMu.Unlock()
	debuglog.Printf("send", "start_lane_hello session=%d lane=%d nonce=%d bytes=%d", sessionID, cfg.LaneID, nonce, len(retryPayload))

	if err := l.WriteTo(ctx, cfg.Leg, packet); err != nil {
		debuglog.Printf("send", "start_lane_write_err session=%d lane=%d leg={%s} err=%v", sessionID, cfg.LaneID, debugLeg(cfg.Leg), err)
		return err
	}
	return nil
}

// getOrCreateLane resolves the lane runtime for key, creating a fresh entry
// (with the supplied initial weight) on first use under lanesMu.
func (l *Send) getOrCreateLane(key laneKey, initialWeight uint32) *laneRuntime {
	l.lanesMu.RLock()
	lane := l.lanes[key]
	l.lanesMu.RUnlock()
	if lane != nil {
		return lane
	}
	l.lanesMu.Lock()
	defer l.lanesMu.Unlock()
	if lane := l.lanes[key]; lane != nil {
		return lane
	}
	lane = newLaneRuntime(key.laneID, initialWeight)
	l.lanes[key] = lane
	return lane
}

// getLane returns the existing lane runtime for key, or nil if absent.
func (l *Send) getLane(key laneKey) *laneRuntime {
	l.lanesMu.RLock()
	defer l.lanesMu.RUnlock()
	return l.lanes[key]
}
