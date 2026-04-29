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
	caps := uint16(l.negotiatedCaps.Load())
	if !ok || caps&protocol.CapTCPFallback == 0 {
		debuglog.Printf("send/probe", "fallback_skip session=%d lane=%d session_nil=%t caps=%#x", key.sessionID, key.laneID, !ok, func() uint16 {
			if !ok {
				return 0
			}
			return caps
		}())
		return
	}
	l.startFallbackDial(ctx, key, lane)
}

func (l *Send) startFallbackDial(ctx context.Context, key laneKey, lane *laneRuntime) {
	streamAvailable := l.streamTransport != nil
	remote, started := lane.tryStartFallback(streamAvailable)
	if !started {
		debuglog.Printf("send/probe", "fallback_dial_skip %s stream_nil=%t", debugLaneState(key, lane), !streamAvailable)
		return
	}

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
		_ = l.handleFallbackDialResult(ctx, fallbackDialResult{
			key: key,
			leg: leg,
			err: err,
		})
	}()
}

func (l *Send) handleFallbackDialResult(ctx context.Context, result fallbackDialResult) error {
	lane := l.getLane(result.key)
	if lane == nil {
		debuglog.Printf("send/probe", "fallback_result_drop missing_lane session=%d lane=%d err=%v", result.key.sessionID, result.key.laneID, result.err)
		return nil
	}
	lane.clearFallbackDialing()
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
	if _, _, ok := l.getSessionState(result.key.sessionID); !ok {
		debuglog.Printf("send/probe", "fallback_result_drop missing_session session=%d lane=%d leg={%s}", result.key.sessionID, result.key.laneID, debugLeg(result.leg))
		return nil
	}

	caps := uint16(l.negotiatedCaps.Load())
	fecProfile := uint8(l.fecProfile.Load())
	helloCaps, helloFEC := lane.helloProfile()
	if caps == 0 && helloCaps != 0 {
		caps = helloCaps
		fecProfile = helloFEC
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
		Weight:     lane.Weight(),
		Leg:        result.leg,
		TCPRemote:  lane.tcpRemoteSnapshot(),
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
		} else {
			l.recordRTTPing(event.Target, event.PingID, event.TimeMS, binding)
		}
	case probe.EventTargetLost:
		return l.handleProbeTargetLost(ctx, event.Target)
	case probe.EventTargetRecovered:
		return l.handleProbeTargetRecovered(event.Target)
	}
	return nil
}

func (l *Send) handleProbeTargetLost(ctx context.Context, target probe.Target) error {
	l.probeMu.Lock()
	binding, ok := l.probeTargets[target]
	l.probeMu.Unlock()
	if !ok {
		debuglog.Printf("send/probe", "target_lost_drop missing_target target=%d", target)
		return nil
	}
	l.clearRTTPendingTarget(target)
	key := laneKey{sessionID: binding.sessionID, laneID: binding.laneID}
	lane := l.getLane(key)
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
		lane.markUDPNotReady()
		l.markRunnableLanesDirty(key.sessionID)
		l.maybeStartFallbackDial(ctx, key, lane)
	case transport.KindTCP:
		lane.markTCPNotReady()
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
	l.probeMu.Lock()
	binding, ok := l.probeTargets[target]
	l.probeMu.Unlock()
	if !ok {
		debuglog.Printf("send/probe", "target_recovered_drop missing_target target=%d", target)
		return nil
	}
	lane := l.getLane(laneKey{sessionID: binding.sessionID, laneID: binding.laneID})
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

	binding := probeBinding{sessionID: sessionID, laneID: laneID, leg: leg}
	pkey := newPingKey(leg)

	l.probeMu.Lock()
	target, existed := l.probeKeys[pkey]
	if !existed {
		target = probe.Target(l.nextProbeTarget.Add(1))
		l.probeKeys[pkey] = target
	}
	l.probeTargets[target] = binding
	l.probeMu.Unlock()

	if existed {
		debuglog.Printf("send/probe", "track_update target=%d session=%d lane=%d leg={%s}", target, sessionID, laneID, debugLeg(leg))
		return
	}
	debuglog.Printf("send/probe", "track target=%d session=%d lane=%d leg={%s}", target, sessionID, laneID, debugLeg(leg))
	l.sendProbeEvent(ctx, probe.Event{Type: probe.EventTrack, Target: target})
}

func (l *Send) untrackProbeTarget(ctx context.Context, leg transport.LegRef) {
	if l.probeEvents == nil {
		debuglog.Printf("send/probe", "untrack_skip probe_events_nil=true leg={%s}", debugLeg(leg))
		return
	}
	pkey := newPingKey(leg)

	l.probeMu.Lock()
	target, ok := l.probeKeys[pkey]
	if ok {
		delete(l.probeKeys, pkey)
		delete(l.probeTargets, target)
	}
	l.probeMu.Unlock()

	if !ok {
		debuglog.Printf("send/probe", "untrack_skip missing_target leg={%s}", debugLeg(leg))
		return
	}
	l.clearRTTPendingTarget(target)
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
