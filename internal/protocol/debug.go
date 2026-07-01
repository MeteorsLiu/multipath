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
		return fmt.Sprintf("%s base_packet_id=%d key=%d source_span=%d repair_count=%d symbol_len=%d", base, body.BasePacketID, body.Key, body.SourceSpan, body.RepairCount, len(body.Symbol))
	case CloseBody:
		return fmt.Sprintf("%s scope=%d reason=%d", base, body.Scope, body.Reason)
	case BandwidthProbeBody:
		return fmt.Sprintf("%s train_id=%d probe_id=%d seq=%d count=%d send_ms=%d train_total=%d train_remaining=%d payload_len=%d", base, body.TrainID, body.ProbeID, body.Seq, body.Count, body.SendMS, body.TrainBytesTotal, body.TrainBytesRemaining, len(body.Payload))
	case BandwidthProbeAckBody:
		return fmt.Sprintf("%s probe_id=%d base_seq=%d count=%d received=%#x first_rx_ms=%d last_rx_ms=%d", base, body.ProbeID, body.BaseSeq, body.Count, body.Received, body.FirstRXMS, body.LastRXMS)
	case LinkStatusBody:
		return fmt.Sprintf("%s status=%#02x udp_delivered_bps=%d tcp_delivered_bps=%d", base, body.Status, body.UDPDeliveredBps, body.TCPDeliveredBps)
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
	case TypeBandwidthProbe:
		return "BW_PROBE"
	case TypeBandwidthProbeAck:
		return "BW_PROBE_ACK"
	case TypeLinkStatus:
		return "LINK_STATUS"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", frameType)
	}
}
