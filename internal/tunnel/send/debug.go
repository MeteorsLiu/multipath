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
		return fmt.Sprintf("%s base_packet_id=%d key=%d symbol_len=%d", base, body.BasePacketID, body.Key, len(body.Symbol))
	case protocol.CloseBody:
		return fmt.Sprintf("%s scope=%d reason=%d", base, body.Scope, body.Reason)
	default:
		return base
	}
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

func debugLaneState(key laneKey, lane *laneRuntime) string {
	if lane == nil {
		return fmt.Sprintf("session=%d lane=%d nil=true", key.sessionID, key.laneID)
	}
	return fmt.Sprintf(
		"session=%d lane=%d weight=%d udp_ready=%t tcp_ready=%t queued=%t fallback_dialing=%t udp={%s} tcp={%s}",
		key.sessionID,
		key.laneID,
		lane.weight,
		lane.udpReady,
		lane.tcpReady,
		lane.queued,
		lane.fallbackDialing,
		debugLeg(lane.udpLeg),
		debugLeg(lane.tcpLeg),
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
