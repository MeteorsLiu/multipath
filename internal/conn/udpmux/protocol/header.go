package protocol

import "encoding/binary"

const MaxHeaderSize = 1 + 2

type PacketType byte

const (
	HeartBeat PacketType = iota + 1
	TunEncap
)

type Header []byte

func MakeHeartBeatHeader(buf []byte) (headerSize int) {
	buf[0] = byte(HeartBeat)
	return 1
}

func MakeEncapHeader(buf []byte, size int) (headerSize int) {
	buf[0] = byte(TunEncap)
	binary.LittleEndian.PutUint16(buf[1:3], uint16(size))
	return 3
}

func (h Header) Type() PacketType {
	return PacketType(h[0])
}

func (h Header) Size() uint16 {
	return binary.LittleEndian.Uint16(h[1:3])
}
