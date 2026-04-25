package send

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func (i *Send) handleTransportFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (Result, error) {
	switch frame.Type {
	case protocol.TypeHELLO:
		body, ok := frame.Body.(protocol.HelloBody)
		if !ok {
			return Result{}, protocol.ErrInvalidFrame
		}
		return i.acceptHello(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypeHELLOACK:
		body, ok := frame.Body.(protocol.HelloAckBody)
		if !ok {
			return Result{}, protocol.ErrInvalidFrame
		}
		return i.acceptHelloAck(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypePING:
		body, ok := frame.Body.(protocol.PingBody)
		if !ok {
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.receivePing(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypePONG:
		body, ok := frame.Body.(protocol.PingBody)
		if !ok {
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.receivePong(ctx, frame.SessionID, frame.LaneID, leg, body)
	case protocol.TypeDATA, protocol.TypeREPAIR:
		accepted, err := i.observeLane(frame.SessionID, frame.LaneID, leg)
		return Result{Accepted: accepted}, err
	case protocol.TypeCLOSE:
		body, ok := frame.Body.(protocol.CloseBody)
		if !ok {
			return Result{}, protocol.ErrInvalidFrame
		}
		return Result{}, i.close(ctx, frame.SessionID, frame.LaneID, body.Scope)
	default:
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

	if laneID == protocol.SessionControlLaneID {
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
		return res, err
	}
	res.Accepted = true
	return res, i.writeHelloAck(ctx, sessionID, laneID, leg, body.Nonce, true, caps, fecProfile)
}

func (i *Send) acceptHelloAck(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.HelloAckBody) (Result, error) {
	caps, fecProfile := negotiateCapabilities(body.Caps, body.FECProfile)
	res := Result{
		Caps:       caps,
		FECProfile: fecProfile,
	}

	session := i.sessions[sessionID]
	if session == nil {
		return res, nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		return res, nil
	}
	if !lane.helloRetry.matches(body.Nonce) {
		return res, nil
	}
	lane.helloRetry.clear()
	if body.Accepted != 1 {
		return res, nil
	}

	session.negotiatedCaps = caps
	session.fecProfile = fecProfile
	lane.observeLeg(leg)
	i.trackProbeTarget(ctx, sessionID, laneID, leg)
	if err := i.enqueueLane(sessionID, lane); err != nil {
		return res, err
	}
	res.Accepted = true
	return res, nil
}

func (i *Send) observeLane(sessionID uint64, laneID uint8, leg transport.LegRef) (bool, error) {
	if i.sessions[sessionID] == nil {
		return false, nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		return false, nil
	}
	lane.observeLeg(leg)
	return true, nil
}

func (i *Send) receivePing(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if i.sessions[sessionID] == nil {
		return nil
	}
	lane := i.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		return nil
	}
	lane.observeLeg(leg)
	return i.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypePONG,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      protocol.PingBody{PingID: body.PingID, TimeMS: body.TimeMS},
	})
}

func (i *Send) receivePong(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
	if i.sessions[sessionID] == nil {
		return nil
	}
	if i.lanes[laneKey{sessionID: sessionID, laneID: laneID}] == nil {
		return nil
	}
	target, ok := i.probeKeys[newPingKey(leg)]
	if !ok {
		return nil
	}
	i.sendProbeEvent(ctx, probe.Event{
		Type:   probe.EventPongReceived,
		Target: target,
		PingID: body.PingID,
		TimeMS: body.TimeMS,
	})
	return nil
}

func (i *Send) close(ctx context.Context, sessionID uint64, laneID uint8, scope uint8) error {
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
		return
	}
	i.untrackProbeTarget(ctx, lane.udpLeg)
	i.untrackProbeTarget(ctx, lane.tcpLeg)
	if i.streamTransport != nil && lane.tcpLeg.ConnID != "" {
		_ = i.streamTransport.Close(ctx, lane.tcpLeg.ConnID)
	}
}

func (i *Send) writeHelloAck(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, nonce uint64, accepted bool, caps uint16, fecProfile uint8) error {
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
			return errLaneUnavailable
		}
	case transport.KindTCP:
		if leg.ConnID == "" {
			return errLaneUnavailable
		}
	default:
		return errLaneUnavailable
	}
	_, err := i.enqueueFrame(ctx, leg, frame)
	return err
}

func (i *Send) writePayloadOnLeg(ctx context.Context, leg transport.LegRef, payload []byte) error {
	switch leg.Kind {
	case transport.KindUDP:
		if leg.EndpointID == "" || leg.RemoteAddr == nil {
			return errLaneUnavailable
		}
	case transport.KindTCP:
		if leg.ConnID == "" {
			return errLaneUnavailable
		}
	default:
		return errLaneUnavailable
	}
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
