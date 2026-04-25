package protocol

import (
	"encoding/binary"
	"errors"
)

var ErrBodyTooShort = errors.New("protocol: frame body too short")

const (
	CapTCPFallback uint16 = 1 << 0
	CapFEC         uint16 = 1 << 1

	FECProfileOff       uint8 = 0
	FECProfileSLC4Plus1 uint8 = 1

	SupportedCaps        = CapTCPFallback | CapFEC
	SessionControlLaneID = 0xff

	CloseScopeLane    uint8 = 1
	CloseScopeSession uint8 = 2
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
	Symbol       []byte
}

func (RepairBody) protocolBody() {}

type CloseBody struct {
	Scope  uint8
	Reason uint8
}

func (CloseBody) protocolBody() {}

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
		return 6 + len(repair.Symbol), nil
	case TypeCLOSE:
		_, ok := frame.Body.(CloseBody)
		return 2, validBody(ok)
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
		copy(out[6:], repair.Symbol)
	case TypeCLOSE:
		body := frame.Body.(CloseBody)
		out[0] = body.Scope
		out[1] = body.Reason
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
		if len(body) < 6 {
			return ErrBodyTooShort
		}
		frame.Body = RepairBody{
			BasePacketID: binary.BigEndian.Uint32(body[:4]),
			Key:          binary.BigEndian.Uint16(body[4:6]),
			Symbol:       body[6:],
		}
	case TypeCLOSE:
		if len(body) != 2 {
			return ErrBodyTooShort
		}
		frame.Body = CloseBody{Scope: body[0], Reason: body[1]}
	default:
		return ErrInvalidFrame
	}
	return nil
}
