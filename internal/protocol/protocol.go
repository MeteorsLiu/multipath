package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

const (
	Version uint8 = 2

	headerSize = 10
)

type FrameType uint8

const (
	TypeHELLO FrameType = iota + 1
	TypeHELLOACK
	TypePING
	TypePONG
	TypeDATA
	TypeREPAIR
	TypeCLOSE
	TypeBandwidthProbe
	TypeBandwidthProbeAck
	TypeLinkStatus
)

var (
	ErrFrameTooShort = errors.New("protocol: frame too short")
	ErrInvalidFrame  = errors.New("protocol: invalid frame")
)

type Frame struct {
	Version   uint8
	Type      FrameType
	SessionID uint64
	LaneID    uint8
	Body      Body
}

func Encode(frame Frame, dst []byte) ([]byte, error) {
	if frame.Version == 0 {
		frame.Version = Version
	}
	if frame.Version != Version {
		err := fmt.Errorf("%w: version out of range", ErrInvalidFrame)
		debugEncodeError(frame, err)
		return nil, err
	}
	if frame.Type == 0 || frame.Type > TypeLinkStatus {
		err := fmt.Errorf("%w: type out of range", ErrInvalidFrame)
		debugEncodeError(frame, err)
		return nil, err
	}

	bodyLen, err := encodedBodySize(frame)
	if err != nil {
		debugEncodeError(frame, err)
		return nil, err
	}

	need := headerSize + bodyLen
	start := len(dst)
	dst = slices.Grow(dst, need)
	dst = dst[:start+need]
	out := dst[start:]

	out[0] = frame.Version<<4 | uint8(frame.Type)
	binary.BigEndian.PutUint64(out[1:9], frame.SessionID)
	out[9] = frame.LaneID
	if err := encodeBodyInto(frame, out[headerSize:]); err != nil {
		debugEncodeError(frame, err)
		return nil, err
	}
	debugEncodeFrame(frame, need)
	return dst, nil
}

func Decode(src []byte) (Frame, error) {
	if len(src) < headerSize {
		debugDecodeError(len(src), ErrFrameTooShort)
		return Frame{}, ErrFrameTooShort
	}

	vt := src[0]
	version := vt >> 4
	frameType := FrameType(vt & 0x0f)
	if version != Version || frameType == 0 || frameType > TypeLinkStatus {
		debugDecodeError(len(src), ErrInvalidFrame)
		return Frame{}, ErrInvalidFrame
	}

	frame := Frame{
		Version:   version,
		Type:      frameType,
		SessionID: binary.BigEndian.Uint64(src[1:9]),
		LaneID:    src[9],
	}
	if err := decodeBody(&frame, src[headerSize:]); err != nil {
		debugDecodeError(len(src), err)
		return Frame{}, err
	}
	debugDecodeFrame(frame, len(src))
	return frame, nil
}
