package send

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

type pingKey struct {
	kind       transport.Kind
	endpointID string
	remote     string
	connID     string
}

func (l *Send) sendPING(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, pingID uint64, timeMS uint64) error {
	lane := l.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
	if lane == nil {
		return errUnknownLane
	}

	ping := protocol.Frame{
		Type:      protocol.TypePING,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      protocol.PingBody{PingID: pingID, TimeMS: timeMS},
	}
	if err := l.writeControlFrameOnLeg(ctx, leg, ping); err != nil {
		return err
	}
	return nil
}

func (l *Send) maybeStartFallbackDial(ctx context.Context, key laneKey, lane *laneRuntime) {
	session := l.sessions[key.sessionID]
	if session == nil || session.negotiatedCaps&protocol.CapTCPFallback == 0 {
		return
	}
	l.startFallbackDial(ctx, key, lane)
}

func (l *Send) startFallbackDial(ctx context.Context, key laneKey, lane *laneRuntime) {
	if l.streamTransport == nil || lane.fallbackDialing || lane.tcpReady || lane.tcpRemote == "" {
		return
	}

	lane.fallbackDialing = true
	remote := lane.tcpRemote
	go func() {
		leg, err := l.streamTransport.Dial(ctx, remote)
		result := fallbackDialResult{
			key: key,
			leg: leg,
			err: err,
		}
		_ = l.finishFallbackDial(ctx, result)
	}()
}

func (l *Send) handleFallbackDialResult(ctx context.Context, result fallbackDialResult) error {
	lane := l.lanes[result.key]
	if lane == nil {
		return nil
	}
	lane.fallbackDialing = false
	if result.err != nil {
		return nil
	}
	session := l.sessions[result.key.sessionID]
	if session == nil {
		return nil
	}

	nonce := l.nextNonce
	l.nextNonce++
	caps := session.negotiatedCaps
	fecProfile := session.fecProfile
	if caps == 0 && lane.helloRetry.caps != 0 {
		caps = lane.helloRetry.caps
		fecProfile = lane.helloRetry.fecProfile
	}
	return l.startLane(ctx, startLaneConfig{
		SessionID:  result.key.sessionID,
		LaneID:     result.key.laneID,
		Weight:     lane.weight,
		Leg:        result.leg,
		TCPRemote:  lane.tcpRemote,
		Nonce:      nonce,
		Caps:       caps,
		FECProfile: fecProfile,
	})
}

func (l *Send) handleProbeEvent(ctx context.Context, event probe.Event) error {
	switch event.Type {
	case probe.EventSendPing:
		binding, ok := l.probeTargets[event.Target]
		if !ok {
			return nil
		}
		if err := l.sendPING(ctx, binding.sessionID, binding.laneID, binding.leg, event.PingID, event.TimeMS); err != nil {
			l.sendProbeEvent(ctx, probe.Event{
				Type:   probe.EventPingFailed,
				Target: event.Target,
				PingID: event.PingID,
				TimeMS: event.TimeMS,
			})
		}
	case probe.EventTargetLost:
		return l.handleProbeTargetLost(ctx, event.Target)
	case probe.EventTargetRecovered:
		return l.handleProbeTargetRecovered(event.Target)
	}
	return nil
}

func (l *Send) handleProbeTargetLost(ctx context.Context, target probe.Target) error {
	binding, ok := l.probeTargets[target]
	if !ok {
		return nil
	}
	key := laneKey{sessionID: binding.sessionID, laneID: binding.laneID}
	lane := l.lanes[key]
	if lane == nil {
		l.untrackProbeTarget(ctx, binding.leg)
		return nil
	}

	switch binding.leg.Kind {
	case transport.KindUDP:
		lane.udpReady = false
		l.maybeStartFallbackDial(ctx, key, lane)
	case transport.KindTCP:
		lane.tcpReady = false
		if l.streamTransport != nil && binding.leg.ConnID != "" {
			_ = l.streamTransport.Close(ctx, binding.leg.ConnID)
		}
		l.untrackProbeTarget(ctx, binding.leg)
	}
	if lane.ready() && !lane.queued {
		return l.enqueueLane(key.sessionID, lane)
	}
	return nil
}

func (l *Send) handleProbeTargetRecovered(target probe.Target) error {
	binding, ok := l.probeTargets[target]
	if !ok {
		return nil
	}
	lane := l.lanes[laneKey{sessionID: binding.sessionID, laneID: binding.laneID}]
	if lane == nil {
		return nil
	}
	lane.observeLeg(binding.leg)
	if lane.ready() && !lane.queued {
		return l.enqueueLane(binding.sessionID, lane)
	}
	return nil
}

func (l *Send) trackProbeTarget(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef) {
	if l.probeEvents == nil || leg.Kind == 0 {
		return
	}
	if l.probeTargets == nil {
		l.probeTargets = make(map[probe.Target]probeBinding)
	}
	if l.probeKeys == nil {
		l.probeKeys = make(map[pingKey]probe.Target)
	}

	key := newPingKey(leg)
	if target, ok := l.probeKeys[key]; ok {
		l.probeTargets[target] = probeBinding{sessionID: sessionID, laneID: laneID, leg: leg}
		return
	}

	l.nextProbeTarget++
	target := l.nextProbeTarget
	l.probeKeys[key] = target
	l.probeTargets[target] = probeBinding{sessionID: sessionID, laneID: laneID, leg: leg}
	l.sendProbeEvent(ctx, probe.Event{Type: probe.EventTrack, Target: target})
}

func (l *Send) untrackProbeTarget(ctx context.Context, leg transport.LegRef) {
	if l.probeEvents == nil || l.probeKeys == nil {
		return
	}
	key := newPingKey(leg)
	target, ok := l.probeKeys[key]
	if !ok {
		return
	}
	delete(l.probeKeys, key)
	delete(l.probeTargets, target)
	l.sendProbeEvent(ctx, probe.Event{Type: probe.EventUntrack, Target: target})
}

func (l *Send) sendProbeEvent(ctx context.Context, event probe.Event) {
	if l.probeEvents == nil {
		return
	}
	select {
	case l.probeEvents <- event:
	case <-ctx.Done():
	}
}

func newPingKey(leg transport.LegRef) pingKey {
	key := pingKey{
		kind:       leg.Kind,
		endpointID: leg.EndpointID,
		connID:     leg.ConnID,
	}
	if leg.RemoteAddr != nil {
		key.remote = leg.RemoteAddr.String()
	}
	return key
}
