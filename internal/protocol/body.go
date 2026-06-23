package protocol

import (
	"encoding/binary"
	"errors"
)

var ErrBodyTooShort = errors.New("protocol: frame body too short")

const (
	CapTCPFallback uint16 = 1 << 0
	CapFEC         uint16 = 1 << 1
	CapLinkStatus  uint16 = 1 << 2

	FECProfileOff              uint8 = 0
	FECProfileSLC4Plus1        uint8 = 1
	FECProfileSLCVariablePlus1 uint8 = 2

	SupportedCaps        = CapFEC | CapLinkStatus
	SessionControlLaneID = 0xff

	CloseScopeLane    uint8 = 1
	CloseScopeSession uint8 = 2

	CloseReasonUnknownSession uint8 = 1

	LinkStatusStateClear   uint8 = 0
	LinkStatusStateLimited uint8 = 1
)

type Body interface {
	protocolBody()
}

type HelloBody struct {
	Nonce      uint64
	Caps       uint16
	FECProfile uint8
}

func (HelloBody) protocolBody() {}

type HelloAckBody struct {
	Nonce      uint64
	Accepted   uint8
	Caps       uint16
	FECProfile uint8
}

func (HelloAckBody) protocolBody() {}

type PingBody struct {
	PingID uint64
	TimeMS uint64
}

func (PingBody) protocolBody() {}

type DataBody struct {
	PacketID uint32
	Packet   []byte
}

func (DataBody) protocolBody() {}

type RepairBody struct {
	BasePacketID uint32
	Key          uint16
	SourceSpan   uint8
	Symbol       []byte
}

func (RepairBody) protocolBody() {}

type CloseBody struct {
	Scope  uint8
	Reason uint8
}

func (CloseBody) protocolBody() {}

type BandwidthProbeBody struct {
	TrainID             uint64
	ProbeID             uint64
	Seq                 uint16
	Count               uint16
	SendMS              uint64
	TrainBytesTotal     uint64
	TrainBytesRemaining uint64
	Payload             []byte
}

func (BandwidthProbeBody) protocolBody() {}

type BandwidthProbeAckBody struct {
	ProbeID   uint64
	BaseSeq   uint16
	Count     uint16
	Received  uint64
	FirstRXMS uint64
	LastRXMS  uint64
}

func (BandwidthProbeAckBody) protocolBody() {}

type LinkStatusBody struct {
	Status          uint8
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
}

func (LinkStatusBody) protocolBody() {}

func encodedBodySize(frame Frame) (int, error) {
	switch frame.Type {
	case TypeHELLO:
		_, ok := frame.Body.(HelloBody)
		return 11, validBody(ok)
	case TypeHELLOACK:
		_, ok := frame.Body.(HelloAckBody)
		return 12, validBody(ok)
	case TypePING, TypePONG:
		_, ok := frame.Body.(PingBody)
		return 16, validBody(ok)
	case TypeDATA:
		data, ok := frame.Body.(DataBody)
		if !ok {
			return 0, ErrInvalidFrame
		}
		return 4 + len(data.Packet), nil
	case TypeREPAIR:
		repair, ok := frame.Body.(RepairBody)
		if !ok {
			return 0, ErrInvalidFrame
		}
		if repair.SourceSpan == 0 || repair.SourceSpan > 4 {
			return 0, ErrInvalidFrame
		}
		return 7 + len(repair.Symbol), nil
	case TypeCLOSE:
		_, ok := frame.Body.(CloseBody)
		return 2, validBody(ok)
	case TypeBandwidthProbe:
		body, ok := frame.Body.(BandwidthProbeBody)
		if !ok || body.Count == 0 || body.Count > 64 || body.Seq >= body.Count ||
			body.TrainBytesTotal == 0 || body.TrainBytesRemaining > body.TrainBytesTotal {
			return 0, ErrInvalidFrame
		}
		return 44 + len(body.Payload), nil
	case TypeBandwidthProbeAck:
		body, ok := frame.Body.(BandwidthProbeAckBody)
		if !ok || body.Count == 0 || body.Count > 64 || body.BaseSeq != 0 {
			return 0, ErrInvalidFrame
		}
		return 36, nil
	case TypeLinkStatus:
		body, ok := frame.Body.(LinkStatusBody)
		if !ok || !validLinkStatusStatus(body.Status) {
			return 0, ErrInvalidFrame
		}
		return 9, nil
	default:
		return 0, ErrInvalidFrame
	}
}

func validBody(ok bool) error {
	if !ok {
		return ErrInvalidFrame
	}
	return nil
}

func encodeBodyInto(frame Frame, out []byte) error {
	switch frame.Type {
	case TypeHELLO:
		body := frame.Body.(HelloBody)
		binary.BigEndian.PutUint64(out[:8], body.Nonce)
		binary.BigEndian.PutUint16(out[8:10], body.Caps)
		out[10] = body.FECProfile
	case TypeHELLOACK:
		body := frame.Body.(HelloAckBody)
		binary.BigEndian.PutUint64(out[:8], body.Nonce)
		out[8] = body.Accepted
		binary.BigEndian.PutUint16(out[9:11], body.Caps)
		out[11] = body.FECProfile
	case TypePING, TypePONG:
		body := frame.Body.(PingBody)
		binary.BigEndian.PutUint64(out[:8], body.PingID)
		binary.BigEndian.PutUint64(out[8:16], body.TimeMS)
	case TypeDATA:
		data := frame.Body.(DataBody)
		binary.BigEndian.PutUint32(out[:4], data.PacketID)
		copy(out[4:], data.Packet)
	case TypeREPAIR:
		repair := frame.Body.(RepairBody)
		binary.BigEndian.PutUint32(out[:4], repair.BasePacketID)
		binary.BigEndian.PutUint16(out[4:6], repair.Key)
		out[6] = repair.SourceSpan
		copy(out[7:], repair.Symbol)
	case TypeCLOSE:
		body := frame.Body.(CloseBody)
		out[0] = body.Scope
		out[1] = body.Reason
	case TypeBandwidthProbe:
		body := frame.Body.(BandwidthProbeBody)
		binary.BigEndian.PutUint64(out[:8], body.TrainID)
		binary.BigEndian.PutUint64(out[8:16], body.ProbeID)
		binary.BigEndian.PutUint16(out[16:18], body.Seq)
		binary.BigEndian.PutUint16(out[18:20], body.Count)
		binary.BigEndian.PutUint64(out[20:28], body.SendMS)
		binary.BigEndian.PutUint64(out[28:36], body.TrainBytesTotal)
		binary.BigEndian.PutUint64(out[36:44], body.TrainBytesRemaining)
		copy(out[44:], body.Payload)
	case TypeBandwidthProbeAck:
		body := frame.Body.(BandwidthProbeAckBody)
		binary.BigEndian.PutUint64(out[:8], body.ProbeID)
		binary.BigEndian.PutUint16(out[8:10], body.BaseSeq)
		binary.BigEndian.PutUint16(out[10:12], body.Count)
		binary.BigEndian.PutUint64(out[12:20], body.Received)
		binary.BigEndian.PutUint64(out[20:28], body.FirstRXMS)
		binary.BigEndian.PutUint64(out[28:36], body.LastRXMS)
	case TypeLinkStatus:
		body := frame.Body.(LinkStatusBody)
		out[0] = body.Status
		binary.BigEndian.PutUint32(out[1:5], body.UDPDeliveredBps)
		binary.BigEndian.PutUint32(out[5:9], body.TCPDeliveredBps)
	default:
		return ErrInvalidFrame
	}
	return nil
}

func decodeBody(frame *Frame, body []byte) error {
	switch frame.Type {
	case TypeHELLO:
		if len(body) != 11 {
			return ErrBodyTooShort
		}
		frame.Body = HelloBody{
			Nonce:      binary.BigEndian.Uint64(body[:8]),
			Caps:       binary.BigEndian.Uint16(body[8:10]),
			FECProfile: body[10],
		}
	case TypeHELLOACK:
		if len(body) != 12 {
			return ErrBodyTooShort
		}
		frame.Body = HelloAckBody{
			Nonce:      binary.BigEndian.Uint64(body[:8]),
			Accepted:   body[8],
			Caps:       binary.BigEndian.Uint16(body[9:11]),
			FECProfile: body[11],
		}
	case TypePING, TypePONG:
		if len(body) != 16 {
			return ErrBodyTooShort
		}
		frame.Body = PingBody{
			PingID: binary.BigEndian.Uint64(body[:8]),
			TimeMS: binary.BigEndian.Uint64(body[8:16]),
		}
	case TypeDATA:
		if len(body) < 4 {
			return ErrBodyTooShort
		}
		frame.Body = DataBody{
			PacketID: binary.BigEndian.Uint32(body[:4]),
			Packet:   body[4:],
		}
	case TypeREPAIR:
		if len(body) < 7 {
			return ErrBodyTooShort
		}
		if body[6] == 0 || body[6] > 4 {
			return ErrInvalidFrame
		}
		frame.Body = RepairBody{
			BasePacketID: binary.BigEndian.Uint32(body[:4]),
			Key:          binary.BigEndian.Uint16(body[4:6]),
			SourceSpan:   body[6],
			Symbol:       body[7:],
		}
	case TypeCLOSE:
		if len(body) != 2 {
			return ErrBodyTooShort
		}
		frame.Body = CloseBody{Scope: body[0], Reason: body[1]}
	case TypeBandwidthProbe:
		if len(body) < 44 {
			return ErrBodyTooShort
		}
		count := binary.BigEndian.Uint16(body[18:20])
		seq := binary.BigEndian.Uint16(body[16:18])
		total := binary.BigEndian.Uint64(body[28:36])
		remaining := binary.BigEndian.Uint64(body[36:44])
		if count == 0 || count > 64 || seq >= count || total == 0 || remaining > total {
			return ErrInvalidFrame
		}
		frame.Body = BandwidthProbeBody{
			TrainID:             binary.BigEndian.Uint64(body[:8]),
			ProbeID:             binary.BigEndian.Uint64(body[8:16]),
			Seq:                 seq,
			Count:               count,
			SendMS:              binary.BigEndian.Uint64(body[20:28]),
			TrainBytesTotal:     total,
			TrainBytesRemaining: remaining,
			Payload:             body[44:],
		}
	case TypeBandwidthProbeAck:
		if len(body) != 36 {
			return ErrBodyTooShort
		}
		baseSeq := binary.BigEndian.Uint16(body[8:10])
		count := binary.BigEndian.Uint16(body[10:12])
		if count == 0 || count > 64 || baseSeq != 0 {
			return ErrInvalidFrame
		}
		frame.Body = BandwidthProbeAckBody{
			ProbeID:   binary.BigEndian.Uint64(body[:8]),
			BaseSeq:   baseSeq,
			Count:     count,
			Received:  binary.BigEndian.Uint64(body[12:20]),
			FirstRXMS: binary.BigEndian.Uint64(body[20:28]),
			LastRXMS:  binary.BigEndian.Uint64(body[28:36]),
		}
	case TypeLinkStatus:
		if len(body) != 9 {
			return ErrBodyTooShort
		}
		status := body[0]
		if !validLinkStatusStatus(status) {
			return ErrInvalidFrame
		}
		frame.Body = LinkStatusBody{
			Status:          status,
			UDPDeliveredBps: binary.BigEndian.Uint32(body[1:5]),
			TCPDeliveredBps: binary.BigEndian.Uint32(body[5:9]),
		}
	default:
		return ErrInvalidFrame
	}
	return nil
}

func validLinkStatusStatus(status uint8) bool {
	udp := status >> 4
	tcp := status & 0x0f
	return validLinkStatusState(udp) && validLinkStatusState(tcp)
}

func validLinkStatusState(state uint8) bool {
	return state <= 7
}
