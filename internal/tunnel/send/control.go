package send

import (
	"context"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func (i *Send) acceptHello(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.HelloBody) error {
	localFEC := uint8(i.fecProfile.Load())
	caps := uint16(0)
	fecProfile := protocol.FECProfileOff
	if laneID != protocol.SessionControlLaneID {
		caps, fecProfile = negotiateCapabilities(body.Caps, body.FECProfile, localFEC)
	}
	debuglog.Printf("send/control", "accept_hello session=%d lane=%d leg={%s} caps=%#x fec_profile=%d negotiated_caps=%#x negotiated_fec_profile=%d", sessionID, laneID, debugLeg(leg), body.Caps, body.FECProfile, caps, fecProfile)

	if laneID == protocol.SessionControlLaneID {
		debuglog.Printf("send/control", "hello_control_lane session=%d lane=%d", sessionID, laneID)
		return i.writeHelloAck(ctx, sessionID, laneID, leg, body.Nonce, false, caps, fecProfile)
	}

	sessionState, _, ok := i.getOrCreateSessionState(sessionID)
	if !ok {
		debuglog.Printf("send/control", "hello_drop session_create_denied session=%d lane=%d", sessionID, laneID)
		return i.writeHelloAck(ctx, sessionID, laneID, leg, body.Nonce, false, caps, fecProfile)
	}
	if oldID, active := i.activeSession(); active && oldID != sessionID {
		i.deleteSessionState(oldID)
		i.closeSessionLanes(ctx, oldID, leg)
	}
	i.activateSession(sessionID)
	i.setBandwidthProbeGateMode(false)
	i.negotiatedCaps.Store(uint32(caps))
	i.fecProfile.Store(uint32(fecProfile))

	key := laneKey{sessionID: sessionID, laneID: laneID}
	lane := i.getOrCreateLane(key, 1)

	before := lane.snapshot()
	oldLeg := currentLegFromSnapshot(before, leg.Kind)
	lane.observeLeg(leg)
	i.markRunnableLanesDirty(sessionID)
	if oldLeg.Kind != 0 && newPingKey(oldLeg) != newPingKey(leg) {
		i.untrackProbeTarget(ctx, oldLeg)
	}
	i.trackProbeTarget(ctx, sessionID, laneID, leg)
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "hello_accept"),
		metrics.L("session", sessionID),
		metrics.L("lane", laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	if shouldLogLaneUp(before, leg) {
		logLaneUp(sessionID, laneID, leg, "hello")
	}
	debuglog.Printf("send/control", "hello_accepted %s", debugLaneState(key, lane))
	return sessionState.Do(func(v sessionpkg.View) error {
		return i.writeHelloAck(ctx, v.SessionID(), laneID, leg, body.Nonce, true, caps, fecProfile)
	})
}

func (i *Send) acceptHelloAck(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.HelloAckBody) error {
	localFEC := uint8(i.fecProfile.Load())
	caps, fecProfile := negotiateCapabilities(body.Caps, body.FECProfile, localFEC)
	debuglog.Printf("send/control", "accept_hello_ack session=%d lane=%d leg={%s} accepted=%d caps=%#x fec_profile=%d negotiated_caps=%#x negotiated_fec_profile=%d", sessionID, laneID, debugLeg(leg), body.Accepted, body.Caps, body.FECProfile, caps, fecProfile)

	sessionState, _, ok := i.getSessionState(sessionID)
	if !ok {
		debuglog.Printf("send/control", "hello_ack_drop missing_session session=%d lane=%d", sessionID, laneID)
		return nil
	}
	key := laneKey{sessionID: sessionID, laneID: laneID}
	lane := i.getLane(key)
	if lane == nil {
		debuglog.Printf("send/control", "hello_ack_drop missing_lane session=%d lane=%d", sessionID, laneID)
		return nil
	}

	// Remove the route under helloRoutesMu, but only if the nonce matches.
	i.helloRoutesMu.Lock()
	route, ok := i.helloRoutes[key]
	if !ok || !route.matches(body.Nonce) {
		i.helloRoutesMu.Unlock()
		debuglog.Printf("send/control", "hello_ack_drop nonce_mismatch session=%d lane=%d nonce=%d", sessionID, laneID, body.Nonce)
		return nil
	}
	delete(i.helloRoutes, key)
	i.helloRoutesMu.Unlock()

	accepted := sessionState.Ack(body.Nonce, body.Accepted == 1)
	if !accepted {
		debuglog.Printf("send/control", "hello_ack_rejected session=%d lane=%d", sessionID, laneID)
		return nil
	}

	i.negotiatedCaps.Store(uint32(caps))
	i.fecProfile.Store(uint32(fecProfile))
	before := lane.snapshot()
	oldLeg := currentLegFromSnapshot(before, leg.Kind)
	lane.observeLeg(leg)
	if leg.Kind == transport.KindTCP {
		lane.resetFallbackFailure()
	} else {
		lane.clearFallbackDialing()
	}
	i.markRunnableLanesDirty(sessionID)
	if oldLeg.Kind != 0 && newPingKey(oldLeg) != newPingKey(leg) {
		i.untrackProbeTarget(ctx, oldLeg)
	}
	i.trackProbeTarget(ctx, sessionID, laneID, leg)
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "hello_ack_accept"),
		metrics.L("session", sessionID),
		metrics.L("lane", laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	if shouldLogLaneUp(before, leg) {
		logLaneUp(sessionID, laneID, leg, "hello_ack")
	}
	debuglog.Printf("send/control", "hello_ack_accepted %s", debugLaneState(laneKey{sessionID: sessionID, laneID: laneID}, lane))
	return nil
}

func (i *Send) receivePing(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if _, _, ok := i.getSessionState(sessionID); !ok {
		debuglog.Printf("send/control", "ping_drop missing_session session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return i.writeUnknownSessionClose(ctx, sessionID, laneID, leg)
	}
	lane := i.getLane(laneKey{sessionID: sessionID, laneID: laneID})
	if lane == nil {
		debuglog.Printf("send/control", "ping_drop missing_lane session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	debuglog.Printf("send/control", "ping session=%d lane=%d ping_id=%d leg={%s}", sessionID, laneID, body.PingID, debugLeg(leg))
	return i.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypePONG,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      protocol.PingBody{PingID: body.PingID, TimeMS: body.TimeMS},
	})
}

func (i *Send) receivePong(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if _, _, ok := i.getSessionState(sessionID); !ok {
		debuglog.Printf("send/control", "pong_drop missing_session session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return i.writeUnknownSessionClose(ctx, sessionID, laneID, leg)
	}
	lane := i.getLane(laneKey{sessionID: sessionID, laneID: laneID})
	if lane == nil {
		debuglog.Printf("send/control", "pong_drop missing_lane session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	i.probeMu.Lock()
	target, ok := i.probeKeys[newPingKey(leg)]
	i.probeMu.Unlock()
	if !ok {
		debuglog.Printf("send/control", "pong_drop missing_target session=%d lane=%d ping_id=%d leg={%s}", sessionID, laneID, body.PingID, debugLeg(leg))
		return nil
	}

	nowMS := uint64(time.Now().UnixMilli())
	if sample, deadlineMS, ok := i.acceptRTTPong(lane, sessionID, laneID, leg, target, body, nowMS); ok {
		debuglog.Printf("send/control", "rtt_sample session=%d lane=%d leg=%s sample_ms=%d srtt_ms=%d rttvar_ms=%d samples=%d", sessionID, laneID, kindMetricLabel(leg.Kind), sample.SampleMS, sample.SRTTMS, sample.RTTVarMS, sample.Samples)

		// Record delivery for leg quality tracking. Pings that timed out are
		// already counted as failures when pruned, so a pong that was rejected
		// here must not add an on-time sample on top of that.
		arrivedOnTime := deadlineMS == 0 || nowMS <= body.TimeMS+deadlineMS
		lane.quality.OnDelivery(leg.Kind, arrivedOnTime)
	}

	debuglog.Printf("send/control", "pong session=%d lane=%d target=%d ping_id=%d leg={%s}", sessionID, laneID, target, body.PingID, debugLeg(leg))
	i.sendProbeEvent(ctx, probe.Event{
		Type:   probe.EventPongReceived,
		Target: target,
		PingID: body.PingID,
		TimeMS: body.TimeMS,
	})
	return nil
}

func (i *Send) close(ctx context.Context, sessionID uint64, laneID uint8, scope uint8, reason uint8) error {
	debuglog.Printf("send/control", "close session=%d lane=%d scope=%d reason=%d", sessionID, laneID, scope, reason)
	switch scope {
	case protocol.CloseScopeLane:
		i.closeLane(ctx, laneKey{sessionID: sessionID, laneID: laneID})
	case protocol.CloseScopeSession:
		wasActive := i.deactivateSessionIfActive(sessionID)
		i.deleteSessionState(sessionID)
		i.closeSessionLanes(ctx, sessionID, transport.LegRef{})
		i.deleteRunnableLanesCache(sessionID)
		if reason == protocol.CloseReasonUnknownSession && wasActive {
			return i.bootstrapNewSession(ctx, "rebootstrap")
		}
	}
	return nil
}

func (i *Send) closeSessionLanes(ctx context.Context, sessionID uint64, keepLeg transport.LegRef) {
	// Snapshot lane keys for this session.
	i.lanesMu.RLock()
	keys := make([]laneKey, 0, len(i.lanes))
	for key := range i.lanes {
		if key.sessionID != sessionID {
			continue
		}
		keys = append(keys, key)
	}
	i.lanesMu.RUnlock()
	for _, key := range keys {
		i.closeLaneKeepingLeg(ctx, key, keepLeg)
	}
}

func (i *Send) writeUnknownSessionClose(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef) error {
	debuglog.Printf("send/control", "close_unknown_session session=%d lane=%d leg={%s}", sessionID, laneID, debugLeg(leg))
	return i.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: sessionID,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.CloseBody{
			Scope:  protocol.CloseScopeSession,
			Reason: protocol.CloseReasonUnknownSession,
		},
	})
}

func (i *Send) closeLane(ctx context.Context, key laneKey) {
	i.closeLaneKeepingLeg(ctx, key, transport.LegRef{})
}

func (i *Send) closeLaneKeepingLeg(ctx context.Context, key laneKey, keepLeg transport.LegRef) {
	i.lanesMu.Lock()
	lane := i.lanes[key]
	delete(i.lanes, key)
	i.lanesMu.Unlock()

	i.markRunnableLanesDirty(key.sessionID)
	if lane == nil {
		debuglog.Printf("send/control", "close_lane missing session=%d lane=%d", key.sessionID, key.laneID)
		return
	}
	i.cancelHelloRoute(key)
	debuglog.Printf("send/control", "close_lane %s", debugLaneState(key, lane))
	udpLeg, tcpLeg := lane.legs()
	i.untrackProbeTarget(ctx, udpLeg)
	i.untrackProbeTarget(ctx, tcpLeg)
	i.clearBandwidthLeg(udpLeg)
	i.clearBandwidthLeg(tcpLeg)
	keepTCP := keepLeg.Kind != 0 && newPingKey(tcpLeg) == newPingKey(keepLeg)
	if i.streamTransport != nil && tcpLeg.ConnID != "" && !keepTCP {
		_ = i.streamTransport.Close(ctx, tcpLeg.ConnID)
	}
}

// cancelHelloRoute removes any pending HELLO route for key and acks the
// corresponding session nonce as rejected.
func (i *Send) cancelHelloRoute(key laneKey) {
	i.helloRoutesMu.Lock()
	route, ok := i.helloRoutes[key]
	if !ok || !route.valid() {
		i.helloRoutesMu.Unlock()
		return
	}
	delete(i.helloRoutes, key)
	i.helloRoutesMu.Unlock()

	if sessionState, ok := i.sessionManager.Get(key.sessionID); ok {
		sessionState.Ack(route.nonce(), false)
	}
}

func (i *Send) writeHelloAck(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, nonce uint64, accepted bool, caps uint16, fecProfile uint8) error {
	debuglog.Printf("send/control", "write_hello_ack session=%d lane=%d accepted=%t caps=%#x fec_profile=%d leg={%s}", sessionID, laneID, accepted, caps, fecProfile, debugLeg(leg))
	return i.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    laneID,
		Body: protocol.HelloAckBody{
			Nonce:      nonce,
			Accepted:   boolByte(accepted),
			Caps:       caps,
			FECProfile: fecProfile,
		},
	})
}

func (i *Send) writeControlFrameOnLeg(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	switch leg.Kind {
	case transport.KindUDP:
		if leg.EndpointID == "" || leg.RemoteAddr == nil {
			debuglog.Printf("send/control", "control_leg_unavailable frame=%s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
			return errLaneUnavailable
		}
	case transport.KindTCP:
		if leg.ConnID == "" {
			debuglog.Printf("send/control", "control_leg_unavailable frame=%s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
			return errLaneUnavailable
		}
	default:
		debuglog.Printf("send/control", "control_leg_unavailable frame=%s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
		return errLaneUnavailable
	}
	debuglog.Printf("send/control", "control_out %s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
	_, err := i.enqueueFrame(ctx, leg, frame)
	return err
}

func (i *Send) writePayloadOnLeg(ctx context.Context, leg transport.LegRef, payload []byte) error {
	switch leg.Kind {
	case transport.KindUDP:
		if leg.EndpointID == "" || leg.RemoteAddr == nil {
			debuglog.Printf("send/control", "payload_leg_unavailable leg={%s} bytes=%d", debugLeg(leg), len(payload))
			return errLaneUnavailable
		}
	case transport.KindTCP:
		if leg.ConnID == "" {
			debuglog.Printf("send/control", "payload_leg_unavailable leg={%s} bytes=%d", debugLeg(leg), len(payload))
			return errLaneUnavailable
		}
	default:
		debuglog.Printf("send/control", "payload_leg_unavailable leg={%s} bytes=%d", debugLeg(leg), len(payload))
		return errLaneUnavailable
	}
	debuglog.Printf("send/control", "payload_out leg={%s} bytes=%d", debugLeg(leg), len(payload))
	return i.enqueuePayload(ctx, leg, payload)
}

func negotiateCapabilities(peerCaps uint16, peerFECProfile uint8, localFECProfile uint8) (uint16, uint8) {
	caps := peerCaps & protocol.SupportedCaps
	if caps&protocol.CapFEC == 0 || !validFECProfile(peerFECProfile) || !validFECProfile(localFECProfile) || peerFECProfile == protocol.FECProfileOff || localFECProfile == protocol.FECProfileOff {
		return caps &^ protocol.CapFEC, protocol.FECProfileOff
	}
	if peerFECProfile < localFECProfile {
		return caps, peerFECProfile
	}
	return caps, localFECProfile
}

func boolByte(ok bool) uint8 {
	if ok {
		return 1
	}
	return 0
}
