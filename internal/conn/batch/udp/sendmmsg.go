package udp

import (
	"net"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var _ conn.BatchWriter = (*SendMmsg)(nil)

type SendMmsg struct {
	cursor int
	conn   *net.UDPConn
	remote *net.UDPAddr

	v4pc *ipv4.PacketConn
	v6pc *ipv6.PacketConn

	v4msgs []ipv4.Message
	v6msgs []ipv6.Message
}

func NewWriterV4(conn net.PacketConn, remoteAddr *net.UDPAddr) *SendMmsg {
	return &SendMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn), remote: remoteAddr}
}

func NewWriterV6(conn net.PacketConn, remoteAddr *net.UDPAddr) *SendMmsg {
	return &SendMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn), remote: remoteAddr}
}

func (s *SendMmsg) lazyInitMsgsV4() error {
	if s.v4msgs == nil {
		s.v4msgs = make([]ipv4.Message, 1024)
		for i := range s.v4msgs {
			s.v4msgs[i].Buffers = make(net.Buffers, 1)
			s.v4msgs[i].Addr = s.remote
		}
	}

	return nil
}

func (s *SendMmsg) lazyInitMsgsV6() error {
	if s.v6msgs == nil {
		s.v6msgs = make([]ipv6.Message, 1024)
		for i := range s.v6msgs {
			s.v6msgs[i].Buffers = make(net.Buffers, 1)
			s.v6msgs[i].Addr = s.remote
		}
	}

	return nil
}

func (s *SendMmsg) resetCursor() {
	for i := range s.v4msgs {
		// ranging over slices is nil safety, while resizing slices is not.
		// s.v4msgs[:s.cursor] will panic when s.v4msgs is nil
		if i >= s.cursor {
			break
		}
		// cut off GC tracking to avoid memory leak.
		s.v4msgs[i].Buffers[0] = nil
	}
	for i := range s.v6msgs {
		if i >= s.cursor {
			break
		}
		s.v6msgs[i].Buffers[0] = nil
	}
	s.cursor = 0
}

func (s *SendMmsg) Write(b []byte) (int, error) {
	if s.v6pc != nil {
		s.lazyInitMsgsV6()
		s.v6msgs[s.cursor].Buffers[0] = b
	} else {
		s.lazyInitMsgsV4()
		s.v4msgs[s.cursor].Buffers[0] = b
	}
	s.cursor++
	return len(b), nil
}

func (s *SendMmsg) Submit() (int64, error) {
	defer s.resetCursor()
	if s.v6pc != nil {
		return s.send6()
	}
	return s.send4()
}

func (s *SendMmsg) send4() (int64, error) {
	var (
		n     int
		start int
		sent  int64
		err   error
	)
	for {
		n, err = s.v4pc.WriteBatch(s.v4msgs[start:s.cursor], 0)
		if err != nil || n == len(s.v4msgs[start:s.cursor]) {
			break
		}
		start += n
	}

	for i := 0; i < start; i++ {
		sent += int64(s.v4msgs[i].N)
	}
	return sent, err
}

func (s *SendMmsg) send6() (int64, error) {
	var (
		n     int
		start int
		sent  int64
		err   error
	)
	for {
		n, err = s.v6pc.WriteBatch(s.v6msgs[start:s.cursor], 0)
		if n > 0 {
			sent += int64(n)
		}
		if err != nil || n == len(s.v6msgs[start:s.cursor]) {
			break
		}
		start += n
	}
	for i := 0; i < start; i++ {
		sent += int64(s.v4msgs[i].N)
	}
	return sent, err
}

func (s *SendMmsg) MessageAt(n int) *ipv4.Message {
	s.lazyInitMsgsV4()
	msg := s.v4msgs[n]
	return &msg
}

func (s *SendMmsg) MessageV6At(n int) *ipv6.Message {
	s.lazyInitMsgsV6()
	msg := s.v6msgs[n]
	return &msg
}
