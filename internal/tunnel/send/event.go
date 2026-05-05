package send

import (
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func logTCPReconnectStart(sessionID uint64, laneID uint8, remote string) {
	eventlog.Printf("tcp_reconnect_start", "session=%d lane=%d remote=%s", sessionID, laneID, remote)
}

func logTCPReconnectFailed(sessionID uint64, laneID uint8, remote string, err error) {
	eventlog.Printf("tcp_reconnect_failed", "session=%d lane=%d remote=%s err=%v", sessionID, laneID, remote, err)
}

func logTCPReconnectConnected(sessionID uint64, laneID uint8, remote string, leg transport.LegRef) {
	eventlog.Printf("tcp_reconnect_connected", "session=%d lane=%d remote=%s ref={%s}", sessionID, laneID, remote, debugLeg(leg))
}

func logLaneUp(sessionID uint64, laneID uint8, leg transport.LegRef, source string) {
	eventlog.Printf("lane_up", "session=%d lane=%d leg=%s source=%s ref={%s}", sessionID, laneID, kindMetricLabel(leg.Kind), source, debugLeg(leg))
}

func logLaneDown(sessionID uint64, laneID uint8, leg transport.LegRef, reason string, err error) {
	if err != nil {
		eventlog.Printf("lane_down", "session=%d lane=%d leg=%s reason=%s err=%v ref={%s}", sessionID, laneID, kindMetricLabel(leg.Kind), reason, err, debugLeg(leg))
		return
	}
	eventlog.Printf("lane_down", "session=%d lane=%d leg=%s reason=%s ref={%s}", sessionID, laneID, kindMetricLabel(leg.Kind), reason, debugLeg(leg))
}

func logLaneHandshakeTimeout(sessionID uint64, laneID uint8, leg transport.LegRef, timeout string) {
	eventlog.Printf("lane_handshake_timeout", "session=%d lane=%d leg=%s timeout=%s ref={%s}", sessionID, laneID, kindMetricLabel(leg.Kind), timeout, debugLeg(leg))
}

func logBandwidthProbeDecision(sessionID uint64, laneID uint8, udp, tcp LegQuality, useUDP bool, ok bool) {
	selected := "none"
	if ok {
		selected = "tcp"
		if useUDP {
			selected = "udp"
		}
	}
	tcpBetter := tcp.BandwidthBps > udp.BandwidthBps
	eventlog.Printf("bandwidth_probe_decision", "session=%d lane=%d udp_active=%t tcp_active=%t udp_bps=%d udp_loss=%.3f udp_samples=%d tcp_bps=%d tcp_loss=%.3f tcp_samples=%d udp_qos_limited=%t tcp_better=%t selected_leg=%s", sessionID, laneID, udp.Active, tcp.Active, udp.BandwidthBps, udp.ProbeLoss, udp.ProbeSamples, tcp.BandwidthBps, tcp.ProbeLoss, tcp.ProbeSamples, udp.BandwidthQoSLimited, tcpBetter, selected)
}

func currentLegFromSnapshot(snap laneSnapshot, kind transport.Kind) transport.LegRef {
	switch kind {
	case transport.KindUDP:
		return snap.udpLeg
	case transport.KindTCP:
		return snap.tcpLeg
	default:
		return transport.LegRef{}
	}
}

func shouldLogLaneUp(before laneSnapshot, leg transport.LegRef) bool {
	return !laneSnapshotReadyOnLeg(before, leg) || !sameLaneSnapshotLeg(before, leg)
}

func shouldLogLaneDown(before laneSnapshot, leg transport.LegRef) bool {
	return laneSnapshotReadyOnLeg(before, leg) && sameLaneSnapshotLeg(before, leg)
}

func laneSnapshotReadyOnLeg(snap laneSnapshot, leg transport.LegRef) bool {
	switch leg.Kind {
	case transport.KindUDP:
		return snap.udpReady && snap.udpLeg.EndpointID != "" && snap.udpLeg.RemoteAddr != nil
	case transport.KindTCP:
		return snap.tcpReady && snap.tcpLeg.ConnID != ""
	default:
		return false
	}
}

func sameLaneSnapshotLeg(snap laneSnapshot, leg transport.LegRef) bool {
	switch leg.Kind {
	case transport.KindUDP:
		return newPingKey(snap.udpLeg) == newPingKey(leg)
	case transport.KindTCP:
		return newPingKey(snap.tcpLeg) == newPingKey(leg)
	default:
		return false
	}
}
