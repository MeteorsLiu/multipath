package send

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func debugFrameSummary(frame protocol.Frame) string {
	base := fmt.Sprintf("type=%s session=%d lane=%d", debugFrameType(frame.Type), frame.SessionID, frame.LaneID)
	switch body := frame.Body.(type) {
	case protocol.HelloBody:
		return fmt.Sprintf("%s nonce=%d caps=%#x fec_profile=%d", base, body.Nonce, body.Caps, body.FECProfile)
	case protocol.HelloAckBody:
		return fmt.Sprintf("%s nonce=%d accepted=%d caps=%#x fec_profile=%d", base, body.Nonce, body.Accepted, body.Caps, body.FECProfile)
	case protocol.PingBody:
		return fmt.Sprintf("%s ping_id=%d time_ms=%d", base, body.PingID, body.TimeMS)
	case protocol.DataBody:
		return fmt.Sprintf("%s packet_id=%d payload_len=%d", base, body.PacketID, len(body.Packet))
	case protocol.RepairBody:
		return fmt.Sprintf("%s base_packet_id=%d key=%d source_span=%d symbol_len=%d", base, body.BasePacketID, body.Key, body.SourceSpan, len(body.Symbol))
	case protocol.CloseBody:
		return fmt.Sprintf("%s scope=%d reason=%d", base, body.Scope, body.Reason)
	case protocol.BandwidthProbeBody:
		return fmt.Sprintf("%s probe_id=%d seq=%d count=%d send_ms=%d payload_len=%d", base, body.ProbeID, body.Seq, body.Count, body.SendMS, len(body.Payload))
	case protocol.BandwidthProbeAckBody:
		return fmt.Sprintf("%s probe_id=%d base_seq=%d count=%d received=%#x first_rx_ms=%d last_rx_ms=%d", base, body.ProbeID, body.BaseSeq, body.Count, body.Received, body.FirstRXMS, body.LastRXMS)
	default:
		return base
	}
}

func debugSuppressFrame(frame protocol.Frame) bool {
	return frame.Type == protocol.TypeBandwidthProbe || frame.Type == protocol.TypeBandwidthProbeAck
}

func debugFrameType(frameType protocol.FrameType) string {
	switch frameType {
	case protocol.TypeHELLO:
		return "HELLO"
	case protocol.TypeHELLOACK:
		return "HELLO_ACK"
	case protocol.TypePING:
		return "PING"
	case protocol.TypePONG:
		return "PONG"
	case protocol.TypeDATA:
		return "DATA"
	case protocol.TypeREPAIR:
		return "REPAIR"
	case protocol.TypeCLOSE:
		return "CLOSE"
	case protocol.TypeBandwidthProbe:
		return "BW_PROBE"
	case protocol.TypeBandwidthProbeAck:
		return "BW_PROBE_ACK"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", frameType)
	}
}

func debugLeg(leg transport.LegRef) string {
	switch leg.Kind {
	case transport.KindUDP:
		remote := "<nil>"
		if leg.RemoteAddr != nil {
			remote = leg.RemoteAddr.String()
		}
		return fmt.Sprintf("udp endpoint=%s remote=%s", leg.EndpointID, remote)
	case transport.KindTCP:
		return fmt.Sprintf("tcp conn=%s", leg.ConnID)
	default:
		return fmt.Sprintf("kind=%d", leg.Kind)
	}
}

func kindMetricLabel(kind transport.Kind) string {
	switch kind {
	case transport.KindUDP:
		return "udp"
	case transport.KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}

func debugLaneState(key laneKey, lane *laneRuntime) string {
	if lane == nil {
		return fmt.Sprintf("session=%d lane=%d nil=true", key.sessionID, key.laneID)
	}
	snap := lane.snapshot()
	return fmt.Sprintf(
		"session=%d lane=%d weight=%d udp_ready=%t tcp_ready=%t fallback_dialing=%t udp={%s} tcp={%s}",
		key.sessionID,
		key.laneID,
		lane.Weight(),
		snap.udpReady,
		snap.tcpReady,
		snap.fallbackDialing,
		debugLeg(snap.udpLeg),
		debugLeg(snap.tcpLeg),
	)
}

func debugProbeEvent(event probe.Event) string {
	return fmt.Sprintf("type=%s target=%d ping_id=%d time_ms=%d", debugProbeEventType(event.Type), event.Target, event.PingID, event.TimeMS)
}

func debugProbeEventType(eventType probe.EventType) string {
	switch eventType {
	case probe.EventTrack:
		return "TRACK"
	case probe.EventUntrack:
		return "UNTRACK"
	case probe.EventSendPing:
		return "SEND_PING"
	case probe.EventPingFailed:
		return "PING_FAILED"
	case probe.EventPongReceived:
		return "PONG_RECEIVED"
	case probe.EventTargetLost:
		return "TARGET_LOST"
	case probe.EventTargetRecovered:
		return "TARGET_RECOVERED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", eventType)
	}
}
