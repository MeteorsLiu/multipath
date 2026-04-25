package send

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func (i *Send) handleTransportFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (Result, error) {
	debuglog.Printf("send/control", "frame_in %s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
	switch frame.Type {
	case protocol.TypeHELLO:
		body, ok := frame.Body.(protocol.HelloBody)
		if !ok {
			debuglog.Printf("send/control", "invalid_body type=HELLO")
			return Result{}, protocol.ErrInvalidFrame
		}
		return i.acceptHello(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypeHELLOACK:
		body, ok := frame.Body.(protocol.HelloAckBody)
		if !ok {
			debuglog.Printf("send/control", "invalid_body type=HELLO_ACK")
			return Result{}, protocol.ErrInvalidFrame
		}
		return i.acceptHelloAck(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypePING:
		body, ok := frame.Body.(protocol.PingBody)
		if !ok {
			debuglog.Printf("send/control", "invalid_body type=PING")
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.receivePing(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypePONG:
		body, ok := frame.Body.(protocol.PingBody)
		if !ok {
			debuglog.Printf("send/control", "invalid_body type=PONG")
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.receivePong(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypeDATA, protocol.TypeREPAIR:
		accepted, err := i.observeLane(frame.SessionID, frame.LaneID, leg)
		debuglog.Printf("send/control", "observe_data accepted=%t err=%v", accepted, err)
		return Result{Accepted: accepted}, err
	case protocol.TypeCLOSE:
		body, ok := frame.Body.(protocol.CloseBody)
		if !ok {
			debuglog.Printf("send/control", "invalid_body type=CLOSE")
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.close(ctx, frame.SessionID, frame.LaneID, body.Scope)
	default:
		debuglog.Printf("send/control", "ignore_unknown type=%d", frame.Type)
		return Result{}, nil
	}
}

func (i *Send) acceptHello(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.HelloBody) (Result, error) {
	caps := uint16(0)
	fecProfile := protocol.FECProfileOff
	if laneID != protocol.SessionControlLaneID {
		caps, fecProfile = negotiateCapabilities(body.Caps, body.FECProfile)
	}
	res := Result{
		Caps:       caps,
		FECProfile: fecProfile,
	}
	debuglog.Printf("send/control", "accept_hello session=%d lane=%d leg={%s} caps=%#x fec_profile=%d negotiated_caps=%#x negotiated_fec_profile=%d", sessionID, laneID, debugLeg(leg), body.Caps, body.FECProfile, caps, fecProfile)

	if laneID == protocol.SessionControlLaneID {
		debuglog.Printf("send/control", "hello_control_lane session=%d lane=%d", sessionID, laneID)
		return res, i.writeHelloAck(ctx, sessionID, laneID, leg, body.Nonce, false, caps, fecProfile)
	}

	session := i.session(sessionID)
	if !i.hasActiveSession {
		i.activateSession(sessionID)
	}
	session.negotiatedCaps = caps
	session.fecProfile = fecProfile

	key := laneKey{sessionID: sessionID, laneID: laneID}
	lane := i.lanes[key]
	if lane == nil {
		lane = newLaneRuntime(laneID, 1)
		i.lanes[key] = lane
	}
	lane.observeLeg(leg)
	i.trackProbeTarget(ctx, sessionID, laneID, leg)
	if err := i.enqueueLane(sessionID, lane); err != nil {
		debuglog.Printf("send/control", "accept_hello enqueue err=%v %s", err, debugLaneState(key, lane))
		return res, err
	}
	res.Accepted = true
	debuglog.Printf("send/control", "hello_accepted %s", debugLaneState(key, lane))
	return res, i.writeHelloAck(ctx, sessionID, laneID, leg, body.Nonce, true, caps, fecProfile)
}

func (i *Send) acceptHelloAck(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.HelloAckBody) (Result, error) {
	caps, fecProfile := negotiateCapabilities(body.Caps, body.FECProfile)
	res := Result{
		Caps:       caps,
		FECProfile: fecProfile,
	}
	debuglog.Printf("send/control", "accept_hello_ack session=%d lane=%d leg={%s} accepted=%d caps=%#x fec_profile=%d negotiated_caps=%#x negotiated_fec_profile=%d", sessionID, laneID, debugLeg(leg), body.Accepted, body.Caps, body.FECProfile, caps, fecProfile)

	session := i.sessions[sessionID]
	if session == nil {
		debuglog.Printf("send/control", "hello_ack_drop missing_session session=%d lane=%d", sessionID, laneID)
		return res, nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		debuglog.Printf("send/control", "hello_ack_drop missing_lane session=%d lane=%d", sessionID, laneID)
		return res, nil
	}
	if !lane.helloRetry.matches(body.Nonce) {
		debuglog.Printf("send/control", "hello_ack_drop nonce_mismatch session=%d lane=%d nonce=%d", sessionID, laneID, body.Nonce)
		return res, nil
	}
	lane.helloRetry.clear()
	if body.Accepted != 1 {
		debuglog.Printf("send/control", "hello_ack_rejected session=%d lane=%d", sessionID, laneID)
		return res, nil
	}

	session.negotiatedCaps = caps
	session.fecProfile = fecProfile
	lane.observeLeg(leg)
	i.trackProbeTarget(ctx, sessionID, laneID, leg)
	if err := i.enqueueLane(sessionID, lane); err != nil {
		debuglog.Printf("send/control", "hello_ack enqueue err=%v %s", err, debugLaneState(laneKey{sessionID: sessionID, laneID: laneID}, lane))
		return res, err
	}
	res.Accepted = true
	debuglog.Printf("send/control", "hello_ack_accepted %s", debugLaneState(laneKey{sessionID: sessionID, laneID: laneID}, lane))
	return res, nil
}

func (i *Send) observeLane(sessionID uint64, laneID uint8, leg transport.LegRef) (bool, error) {
	if i.sessions[sessionID] == nil {
		debuglog.Printf("send/control", "observe_lane_drop missing_session session=%d lane=%d leg={%s}", sessionID, laneID, debugLeg(leg))
		return false, nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		debuglog.Printf("send/control", "observe_lane_drop missing_lane session=%d lane=%d leg={%s}", sessionID, laneID, debugLeg(leg))
		return false, nil
	}
	lane.observeLeg(leg)
	debuglog.Printf("send/control", "observe_lane %s", debugLaneState(laneKey{sessionID: sessionID, laneID: laneID}, lane))
	return true, nil
}

func (i *Send) receivePing(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if i.sessions[sessionID] == nil {
		debuglog.Printf("send/control", "ping_drop missing_session session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		debuglog.Printf("send/control", "ping_drop missing_lane session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	lane.observeLeg(leg)
	debuglog.Printf("send/control", "ping session=%d lane=%d ping_id=%d leg={%s}", sessionID, laneID, body.PingID, debugLeg(leg))
	return i.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypePONG,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      protocol.PingBody{PingID: body.PingID, TimeMS: body.TimeMS},
	})
}

func (i *Send) receivePong(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if i.sessions[sessionID] == nil {
		debuglog.Printf("send/control", "pong_drop missing_session session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	if i.lanes[laneKey{sessionID: sessionID, laneID: laneID}] == nil {
		debuglog.Printf("send/control", "pong_drop missing_lane session=%d lane=%d ping_id=%d", sessionID, laneID, body.PingID)
		return nil
	}
	target, ok := i.probeKeys[newPingKey(leg)]
	if !ok {
		debuglog.Printf("send/control", "pong_drop missing_target session=%d lane=%d ping_id=%d leg={%s}", sessionID, laneID, body.PingID, debugLeg(leg))
		return nil
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

func (i *Send) close(ctx context.Context, sessionID uint64, laneID uint8, scope uint8) error {
	debuglog.Printf("send/control", "close session=%d lane=%d scope=%d", sessionID, laneID, scope)
	switch scope {
	case protocol.CloseScopeLane:
		i.closeLane(ctx, laneKey{sessionID: sessionID, laneID: laneID})
	case protocol.CloseScopeSession:
		delete(i.sessions, sessionID)
		delete(i.schedulers, sessionID)
		if i.hasActiveSession && i.activeSessionID == sessionID {
			i.hasActiveSession = false
			i.activeSessionID = 0
		}
		for key := range i.lanes {
			if key.sessionID == sessionID {
				i.closeLane(ctx, key)
			}
		}
	}
	return nil
}

func (i *Send) closeLane(ctx context.Context, key laneKey) {
	lane := i.lanes[key]
	delete(i.lanes, key)
	if lane == nil {
		debuglog.Printf("send/control", "close_lane missing session=%d lane=%d", key.sessionID, key.laneID)
		return
	}
	debuglog.Printf("send/control", "close_lane %s", debugLaneState(key, lane))
	i.untrackProbeTarget(ctx, lane.udpLeg)
	i.untrackProbeTarget(ctx, lane.tcpLeg)
	if i.streamTransport != nil && lane.tcpLeg.ConnID != "" {
		_ = i.streamTransport.Close(ctx, lane.tcpLeg.ConnID)
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

func negotiateCapabilities(peerCaps uint16, peerFECProfile uint8) (uint16, uint8) {
	caps := peerCaps & protocol.SupportedCaps
	if caps&protocol.CapFEC == 0 || peerFECProfile != protocol.FECProfileSLC4Plus1 {
		return caps &^ protocol.CapFEC, protocol.FECProfileOff
	}
	return caps, protocol.FECProfileSLC4Plus1
}

func boolByte(ok bool) uint8 {
	if ok {
		return 1
	}
	return 0
}
