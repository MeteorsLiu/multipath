package protocol

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

func MakeHeader(buf []byte, pktType PacketType) (headerSize int) {
	buf[0] = byte(pktType)
	return 1
}

func (h Header) Type() PacketType {
	return PacketType(h[0])
}
