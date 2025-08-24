package protocol

import "io"

const HeaderSize = 1

type PacketType byte

const (
	HeartBeat PacketType = iota + 1
	TunEncap
)

func (p PacketType) String() string {
	switch p {
	case HeartBeat:
		return "heartbeat"
	case TunEncap:
		return "tun"
	}
	return "unknown"
}

type Header []byte

func MakeHeader(writer io.WriterAt, pktType PacketType) (headerSize int) {
	var header [1]byte
	header[0] = byte(pktType)
	headerSize, _ = writer.WriteAt(header[:], 0)
	return
}

func (h Header) Type() PacketType {
	return PacketType(h[0])
}
