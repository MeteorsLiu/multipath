package udp

import (
	"net"
	"runtime"

	"github.com/MeteorsLiu/multipath/internal/conn"
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

var _ conn.BatchReader = (*RecvMmsg)(nil)

func NewReaderV4(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn)}
}

func NewReaderV6(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn)}
}

func (s *RecvMmsg) MessageAt(n int) ipv4.Message {
	return s.v4msgs[n]
}
func (s *RecvMmsg) MessageV6At(n int) ipv6.Message {
	return s.v6msgs[n]
}

func (s *RecvMmsg) RemoteAddrsV6() []ipv6.Message {
	return s.v6msgs
}

func (s *RecvMmsg) ReadBatch(b [][]byte) (int, int64, error) {
	if s.v6pc != nil {
		return s.readBatchV6(b)
	}
	return s.readBatchV4(b)
}

func (s *RecvMmsg) fillMsgsV4(bufs [][]byte) error {
	if len(bufs) > 1024 {
		return conn.ErrTooManySegments
	}
	if s.v4msgs == nil {
		s.v4msgs = make([]ipv4.Message, 1024)
		for i := range s.v4msgs {
			s.v4msgs[i].Buffers = make(net.Buffers, 1)
		}
	}
	for i := range bufs {
		s.v4msgs[i].Buffers[0] = bufs[i]
	}

	return nil
}

func (s *RecvMmsg) fillMsgsV6(bufs [][]byte) error {
	if len(bufs) > 1024 {
		return conn.ErrTooManySegments
	}
	if s.v6msgs == nil {
		s.v6msgs = make([]ipv6.Message, 1024)
		for i := range s.v6msgs {
			s.v6msgs[i].Buffers = make(net.Buffers, 1)
		}
	}
	for i := range bufs {
		s.v6msgs[i].Buffers[0] = bufs[i]
	}

	return nil
}

func (s *RecvMmsg) readBatchV4(bufs [][]byte) (int, int64, error) {
	err := s.fillMsgsV4(bufs)
	if err != nil {
		return 0, 0, err
	}

	var numMsgs int

	if runtime.GOOS == "linux" {
		numMsgs, err = s.v4pc.ReadBatch(s.v4msgs, 0)
		if err != nil {
			return 0, 0, err
		}
	} else {
		s.v4msgs[0].N, s.v4msgs[0].NN, _, s.v4msgs[0].Addr, err = s.conn.ReadMsgUDP(s.v4msgs[0].Buffers[0], s.v4msgs[0].OOB)
		if err != nil {
			return 0, 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		sum += int64(s.v4msgs[i].N)
	}

	return numMsgs, sum, nil
}

func (s *RecvMmsg) readBatchV6(bufs [][]byte) (int, int64, error) {
	err := s.fillMsgsV6(bufs)
	if err != nil {
		return 0, 0, err
	}

	var numMsgs int

	if runtime.GOOS == "linux" {
		numMsgs, err = s.v6pc.ReadBatch(s.v4msgs, 0)
		if err != nil {
			return 0, 0, err
		}
	} else {
		s.v6msgs[0].N, s.v6msgs[0].NN, _, s.v6msgs[0].Addr, err = s.conn.ReadMsgUDP(s.v6msgs[0].Buffers[0], s.v6msgs[0].OOB)
		if err != nil {
			return 0, 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		sum += int64(s.v4msgs[i].N)
	}

	return numMsgs, sum, nil
}
