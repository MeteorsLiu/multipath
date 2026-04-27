package send

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
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
		debuglog.Printf("send/probe", "send_ping err=%v session=%d lane=%d ping_id=%d leg={%s}", err, sessionID, laneID, pingID, debugLeg(leg))
		return err
	}
	debuglog.Printf("send/probe", "send_ping session=%d lane=%d ping_id=%d leg={%s}", sessionID, laneID, pingID, debugLeg(leg))
	return nil
}

func (l *Send) maybeStartFallbackDial(ctx context.Context, key laneKey, lane *laneRuntime) {
	_, _, ok := l.getSessionState(key.sessionID)
	if !ok || l.negotiatedCaps&protocol.CapTCPFallback == 0 {
		debuglog.Printf("send/probe", "fallback_skip session=%d lane=%d session_nil=%t caps=%#x", key.sessionID, key.laneID, !ok, func() uint16 {
			if !ok {
				return 0
			}
			return l.negotiatedCaps
		}())
		return
	}
	l.startFallbackDial(ctx, key, lane)
}

func (l *Send) startFallbackDial(ctx context.Context, key laneKey, lane *laneRuntime) {
	if l.streamTransport == nil || lane.fallbackDialing || lane.tcpReady || lane.tcpRemote == "" {
		debuglog.Printf("send/probe", "fallback_dial_skip %s stream_nil=%t", debugLaneState(key, lane), l.streamTransport == nil)
		return
	}

	lane.fallbackDialing = true
	remote := lane.tcpRemote
	debuglog.Printf("send/probe", "fallback_dial_start session=%d lane=%d remote=%s", key.sessionID, key.laneID, remote)
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "fallback_dial_start"),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", "tcp"),
	)
	go func() {
		leg, err := l.streamTransport.Dial(ctx, remote)
		debuglog.Printf("send/probe", "fallback_dial_done session=%d lane=%d remote=%s leg={%s} err=%v", key.sessionID, key.laneID, remote, debugLeg(leg), err)
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
		debuglog.Printf("send/probe", "fallback_result_drop missing_lane session=%d lane=%d err=%v", result.key.sessionID, result.key.laneID, result.err)
		return nil
	}
	lane.fallbackDialing = false
	if result.err != nil {
		debuglog.Printf("send/probe", "fallback_result_err %s err=%v", debugLaneState(result.key, lane), result.err)
		metrics.IncCounter(metrics.LaneEventsTotal,
			metrics.L("event", "fallback_dial_error"),
			metrics.L("session", result.key.sessionID),
			metrics.L("lane", result.key.laneID),
			metrics.L("leg", "tcp"),
		)
		return nil
	}
	_, _, ok := l.getSessionState(result.key.sessionID)
	if !ok {
		debuglog.Printf("send/probe", "fallback_result_drop missing_session session=%d lane=%d leg={%s}", result.key.sessionID, result.key.laneID, debugLeg(result.leg))
		return nil
	}

	caps := l.negotiatedCaps
	fecProfile := l.fecProfile
	if caps == 0 && lane.helloCaps != 0 {
		caps = lane.helloCaps
		fecProfile = lane.helloFECProfile
	}
	debuglog.Printf("send/probe", "fallback_result_start_lane session=%d lane=%d leg={%s} caps=%#x fec_profile=%d", result.key.sessionID, result.key.laneID, debugLeg(result.leg), caps, fecProfile)
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "fallback_dial_success"),
		metrics.L("session", result.key.sessionID),
		metrics.L("lane", result.key.laneID),
		metrics.L("leg", kindMetricLabel(result.leg.Kind)),
	)
	return l.startLane(ctx, startLaneConfig{
		SessionID:  result.key.sessionID,
		LaneID:     result.key.laneID,
		Weight:     lane.weight,
		Leg:        result.leg,
		TCPRemote:  lane.tcpRemote,
		Caps:       caps,
		FECProfile: fecProfile,
	})
}

func (l *Send) handleProbeEvent(ctx context.Context, event probe.Event) error {
	debuglog.Printf("send/probe", "event %s", debugProbeEvent(event))
	metrics.IncCounter(metrics.ProbeEventsTotal,
		metrics.L("event", debugProbeEventType(event.Type)),
		metrics.L("direction", "in"),
	)
	switch event.Type {
	case probe.EventSendPing:
		binding, ok := l.probeTargets[event.Target]
		if !ok {
			debuglog.Printf("send/probe", "event_drop missing_target %s", debugProbeEvent(event))
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
		debuglog.Printf("send/probe", "target_lost_drop missing_target target=%d", target)
		return nil
	}
	key := laneKey{sessionID: binding.sessionID, laneID: binding.laneID}
	lane := l.lanes[key]
	if lane == nil {
		debuglog.Printf("send/probe", "target_lost_drop missing_lane target=%d session=%d lane=%d leg={%s}", target, binding.sessionID, binding.laneID, debugLeg(binding.leg))
		l.untrackProbeTarget(ctx, binding.leg)
		return nil
	}
	debuglog.Printf("send/probe", "target_lost target=%d leg={%s} before=%s", target, debugLeg(binding.leg), debugLaneState(key, lane))
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "target_lost"),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(binding.leg.Kind)),
	)

	switch binding.leg.Kind {
	case transport.KindUDP:
		lane.udpReady = false
		l.markRunnableLanesDirty(key.sessionID)
		l.maybeStartFallbackDial(ctx, key, lane)
	case transport.KindTCP:
		lane.tcpReady = false
		l.markRunnableLanesDirty(key.sessionID)
		if l.streamTransport != nil && binding.leg.ConnID != "" {
			_ = l.streamTransport.Close(ctx, binding.leg.ConnID)
		}
		l.untrackProbeTarget(ctx, binding.leg)
	}
	debuglog.Printf("send/probe", "target_lost_done %s", debugLaneState(key, lane))
	return nil
}

func (l *Send) handleProbeTargetRecovered(target probe.Target) error {
	binding, ok := l.probeTargets[target]
	if !ok {
		debuglog.Printf("send/probe", "target_recovered_drop missing_target target=%d", target)
		return nil
	}
	lane := l.lanes[laneKey{sessionID: binding.sessionID, laneID: binding.laneID}]
	if lane == nil {
		debuglog.Printf("send/probe", "target_recovered_drop missing_lane target=%d session=%d lane=%d leg={%s}", target, binding.sessionID, binding.laneID, debugLeg(binding.leg))
		return nil
	}
	lane.observeLeg(binding.leg)
	l.markRunnableLanesDirty(binding.sessionID)
	metrics.IncCounter(metrics.LaneEventsTotal,
		metrics.L("event", "target_recovered"),
		metrics.L("session", binding.sessionID),
		metrics.L("lane", binding.laneID),
		metrics.L("leg", kindMetricLabel(binding.leg.Kind)),
	)
	debuglog.Printf("send/probe", "target_recovered target=%d %s", target, debugLaneState(laneKey{sessionID: binding.sessionID, laneID: binding.laneID}, lane))
	return nil
}

func (l *Send) trackProbeTarget(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef) {
	if l.probeEvents == nil || leg.Kind == 0 {
		debuglog.Printf("send/probe", "track_skip session=%d lane=%d probe_events_nil=%t leg={%s}", sessionID, laneID, l.probeEvents == nil, debugLeg(leg))
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
		debuglog.Printf("send/probe", "track_update target=%d session=%d lane=%d leg={%s}", target, sessionID, laneID, debugLeg(leg))
		return
	}

	l.nextProbeTarget++
	target := l.nextProbeTarget
	l.probeKeys[key] = target
	l.probeTargets[target] = probeBinding{sessionID: sessionID, laneID: laneID, leg: leg}
	debuglog.Printf("send/probe", "track target=%d session=%d lane=%d leg={%s}", target, sessionID, laneID, debugLeg(leg))
	l.sendProbeEvent(ctx, probe.Event{Type: probe.EventTrack, Target: target})
}

func (l *Send) untrackProbeTarget(ctx context.Context, leg transport.LegRef) {
	if l.probeEvents == nil || l.probeKeys == nil {
		debuglog.Printf("send/probe", "untrack_skip probe_events_nil=%t probe_keys_nil=%t leg={%s}", l.probeEvents == nil, l.probeKeys == nil, debugLeg(leg))
		return
	}
	key := newPingKey(leg)
	target, ok := l.probeKeys[key]
	if !ok {
		debuglog.Printf("send/probe", "untrack_skip missing_target leg={%s}", debugLeg(leg))
		return
	}
	delete(l.probeKeys, key)
	delete(l.probeTargets, target)
	debuglog.Printf("send/probe", "untrack target=%d leg={%s}", target, debugLeg(leg))
	l.sendProbeEvent(ctx, probe.Event{Type: probe.EventUntrack, Target: target})
}

func (l *Send) sendProbeEvent(ctx context.Context, event probe.Event) {
	if l.probeEvents == nil {
		return
	}
	debuglog.Printf("send/probe", "event_out %s", debugProbeEvent(event))
	metrics.IncCounter(metrics.ProbeEventsTotal,
		metrics.L("event", debugProbeEventType(event.Type)),
		metrics.L("direction", "out"),
	)
	select {
	case l.probeEvents <- event:
	case <-ctx.Done():
		debuglog.Printf("send/probe", "event_out_drop ctx_done %s", debugProbeEvent(event))
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
