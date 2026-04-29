package protocol

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
)

func debugEncodeFrame(frame Frame, frameLen int) {
	if !debuglog.Enabled() {
		return
	}
	debuglog.Printf("protocol", "encode %s frame_len=%d", debugFrame(frame), frameLen)
}

func debugEncodeError(frame Frame, err error) {
	if !debuglog.Enabled() {
		return
	}
	debuglog.Printf("protocol", "encode_error %s err=%v", debugFrame(frame), err)
}

func debugDecodeFrame(frame Frame, frameLen int) {
	if !debuglog.Enabled() {
		return
	}
	debuglog.Printf("protocol", "decode %s frame_len=%d", debugFrame(frame), frameLen)
}

func debugDecodeError(srcLen int, err error) {
	if !debuglog.Enabled() {
		return
	}
	debuglog.Printf("protocol", "decode_error len=%d err=%v", srcLen, err)
}

func debugFrame(frame Frame) string {
	base := fmt.Sprintf("type=%s session=%d lane=%d", frameTypeName(frame.Type), frame.SessionID, frame.LaneID)
	switch body := frame.Body.(type) {
	case HelloBody:
		return fmt.Sprintf("%s nonce=%d caps=%#x fec_profile=%d", base, body.Nonce, body.Caps, body.FECProfile)
	case HelloAckBody:
		return fmt.Sprintf("%s nonce=%d accepted=%d caps=%#x fec_profile=%d", base, body.Nonce, body.Accepted, body.Caps, body.FECProfile)
	case PingBody:
		return fmt.Sprintf("%s ping_id=%d time_ms=%d", base, body.PingID, body.TimeMS)
	case DataBody:
		return fmt.Sprintf("%s packet_id=%d payload_len=%d", base, body.PacketID, len(body.Packet))
	case RepairBody:
		return fmt.Sprintf("%s base_packet_id=%d key=%d source_span=%d symbol_len=%d", base, body.BasePacketID, body.Key, body.SourceSpan, len(body.Symbol))
	case CloseBody:
		return fmt.Sprintf("%s scope=%d reason=%d", base, body.Scope, body.Reason)
	default:
		return base
	}
}

func frameTypeName(frameType FrameType) string {
	switch frameType {
	case TypeHELLO:
		return "HELLO"
	case TypeHELLOACK:
		return "HELLO_ACK"
	case TypePING:
		return "PING"
	case TypePONG:
		return "PONG"
	case TypeDATA:
		return "DATA"
	case TypeREPAIR:
		return "REPAIR"
	case TypeCLOSE:
		return "CLOSE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", frameType)
	}
}
