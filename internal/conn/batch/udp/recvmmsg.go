package udp

import (
	"net"
	"runtime"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	ipv4MsgsPool = sync.Pool{
		New: func() any {
			msgs := make([]ipv4.Message, 1024)
			for i := range msgs {
				msgs[i].Buffers = make(net.Buffers, 1)
			}
			return &msgs
		},
	}

	ipv6MsgsPool = sync.Pool{
		New: func() any {
			msgs := make([]ipv6.Message, 1024)
			for i := range msgs {
				msgs[i].Buffers = make(net.Buffers, 1)
			}
			return &msgs
		},
	}
)

type RecvMmsg struct {
	conn *net.UDPConn

	v4pc *ipv4.PacketConn
	v6pc *ipv6.PacketConn
}

func NewReaderV4(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v4pc: ipv4.NewPacketConn(conn)}
}

func NewReaderV6(conn net.PacketConn) *RecvMmsg {
	return &RecvMmsg{conn: conn.(*net.UDPConn), v6pc: ipv6.NewPacketConn(conn)}
}

func (s *RecvMmsg) ReadBatch(b [][]byte) (int64, error) {
	if s.v6pc != nil {
		return s.readBatchV6(b)
	}
	return s.readBatchV4(b)
}

func fillMsgsV4(bufs [][]byte) (*[]ipv4.Message, error) {
	if len(bufs) > 1024 {
		return nil, conn.ErrTooManySegments
	}
	msgs := ipv4MsgsPool.Get().(*[]ipv4.Message)

	for i := range bufs {
		(*msgs)[i].Buffers[0] = bufs[i]
	}

	return msgs, nil
}

func fillMsgsV6(bufs [][]byte) (*[]ipv4.Message, error) {
	if len(bufs) > 1024 {
		return nil, conn.ErrTooManySegments
	}
	msgs := ipv6MsgsPool.Get().(*[]ipv6.Message)

	for i := range bufs {
		(*msgs)[i].Buffers[0] = bufs[i]
	}

	return msgs, nil
}

func (s *RecvMmsg) readBatchV4(bufs [][]byte) (int64, error) {
	msgs, err := fillMsgsV4(bufs)
	if err != nil {
		return 0, err
	}
	defer ipv4MsgsPool.Put(msgs)

	var numMsgs int
	if runtime.GOOS == "linux" {
		numMsgs, err = s.v4pc.ReadBatch(*msgs, 0)
		if err != nil {
			return 0, err
		}
	} else {
		msg := &(*msgs)[0]
		msg.N, msg.NN, _, msg.Addr, err = s.conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
		if err != nil {
			return 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		msg := &(*msgs)[i]
		sum += int64(msg.N)
	}

	return sum, nil
}

func (s *RecvMmsg) readBatchV6(bufs [][]byte) (int64, error) {
	msgs, err := fillMsgsV6(bufs)
	if err != nil {
		return 0, err
	}
	defer ipv6MsgsPool.Put(msgs)

	var numMsgs int
	if runtime.GOOS == "linux" {
		numMsgs, err = s.v6pc.ReadBatch(*msgs, 0)
		if err != nil {
			return 0, err
		}
	} else {
		msg := &(*msgs)[0]
		msg.N, msg.NN, _, msg.Addr, err = s.conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
		if err != nil {
			return 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		msg := &(*msgs)[i]
		sum += int64(msg.N)
	}

	return sum, nil
}
