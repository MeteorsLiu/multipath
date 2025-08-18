package udp

import (
	"net"
	"runtime"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type SendMmsg struct {
	conn   *net.UDPConn
	remote *net.UDPAddr

	v4pc *ipv4.PacketConn
	v6pc *ipv6.PacketConn

	v4msgs []ipv4.Message
	v6msgs []ipv4.Message
}

func NewWriterV4(conn net.PacketConn, remoteAddr *net.UDPAddr) *SendMmsg {
	return &SendMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn), remote: remoteAddr}
}

func NewWriterV6(conn net.PacketConn, remoteAddr *net.UDPAddr) *SendMmsg {
	return &SendMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn), remote: remoteAddr}
}

func (s *SendMmsg) fillMsgsV4(bufs [][]byte) error {
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
		s.v4msgs[i].Addr = s.remote
	}

	return nil
}

func (s *SendMmsg) fillMsgsV6(bufs [][]byte) error {
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
		s.v6msgs[i].Addr = s.remote
	}

	return nil
}

func (s *SendMmsg) WriteBatch(bufs [][]byte) (int64, error) {
	if s.v6pc != nil {
		return s.send6(bufs)
	}
	return s.send4(bufs)
}

func (s *SendMmsg) send4(bufs [][]byte) (int64, error) {
	err := s.fillMsgsV4(bufs)
	if err != nil {
		return 0, err
	}
	var (
		n     int
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v4pc.WriteBatch(s.v4msgs[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len(s.v4msgs[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, s.v4msgs[i].OOB, s.remote)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil {
				break
			}
		}
	}
	return sent, err
}

func (s *SendMmsg) send6(bufs [][]byte) (int64, error) {
	err := s.fillMsgsV6(bufs)
	if err != nil {
		return 0, err
	}

	var (
		n     int
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v6pc.WriteBatch(s.v6msgs[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len(s.v6msgs[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, s.v6msgs[i].OOB, s.remote)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil {
				break
			}
		}
	}
	return sent, err
}
