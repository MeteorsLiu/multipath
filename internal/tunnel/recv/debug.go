package recv

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
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
