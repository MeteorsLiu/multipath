package udp

import (
	"net"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type RecvMmsg struct {
	conn *net.UDPConn

	v4pc *ipv4.PacketConn
	v6pc *ipv6.PacketConn

	v4msgs []ipv4.Message
	v6msgs []ipv4.Message
}

func NewReaderV4(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn)}
}

func NewReaderV6(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn)}
}

func (s *RecvMmsg) MessageAt(n int) *ipv4.Message {
	s.lazyInitMsgsV4()
	return &s.v4msgs[n]
}
func (s *RecvMmsg) MessageV6At(n int) *ipv6.Message {
	s.lazyInitMsgsV6()
	return &s.v6msgs[n]
}

func (s *RecvMmsg) ReadMessage() (int, int64, error) {
	if s.v6pc != nil {
		return s.readBatchV6()
	}
	return s.readBatchV4()
}

func (s *RecvMmsg) lazyInitMsgsV4() {
	if s.v4msgs == nil {
		s.v4msgs = make([]ipv4.Message, 1024)
		for i := range s.v4msgs {
			s.v4msgs[i].Buffers = make(net.Buffers, 1)
		}
	}
}

func (s *RecvMmsg) lazyInitMsgsV6() {
	if s.v6msgs == nil {
		s.v6msgs = make([]ipv6.Message, 1024)
		for i := range s.v6msgs {
			s.v6msgs[i].Buffers = make(net.Buffers, 1)
		}
	}
}

func (s *RecvMmsg) readBatchV4() (int, int64, error) {
	var err error
	var numMsgs int

	numMsgs, err = s.v4pc.ReadBatch(s.v4msgs, 0)
	if err != nil {
		return 0, 0, err
	}
	return numMsgs, 0, nil
}

func (s *RecvMmsg) readBatchV6() (int, int64, error) {
	var err error
	var numMsgs int

	numMsgs, err = s.v6pc.ReadBatch(s.v4msgs, 0)
	if err != nil {
		return 0, 0, err
	}

	return numMsgs, 0, nil
}
