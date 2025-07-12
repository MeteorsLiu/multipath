package udp

import (
	"net"
	"runtime"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type sendMmsg struct {
	conn *net.UDPConn

	v4pc *ipv4.PacketConn
	v6pc *ipv6.PacketConn
}

func NewWriterV4(conn net.PacketConn) conn.BatchWriter {
	return &sendMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn)}
}

func NewWriterV6(conn net.PacketConn) conn.BatchWriter {
	return &sendMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn)}
}

func (s *sendMmsg) WriteBatch(bufs [][]byte) (int64, error) {
	if s.v6pc != nil {
		return s.send6(bufs)
	}
	return s.send4(bufs)
}

func (s *sendMmsg) send4(bufs [][]byte) (int64, error) {
	msgs, err := fillMsgsV4(bufs)
	if err != nil {
		return 0, err
	}
	defer ipv4MsgsPool.Put(msgs)

	var (
		n     int
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v4pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, (*msgs)[i].OOB, nil)
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

func (s *sendMmsg) send6(bufs [][]byte) (int64, error) {
	msgs, err := fillMsgsV6(bufs)
	if err != nil {
		return 0, err
	}
	defer ipv6MsgsPool.Put(msgs)

	var (
		n     int
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v6pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, (*msgs)[i].OOB, nil)
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
