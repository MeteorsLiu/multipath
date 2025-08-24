package udpmux

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type udpSender struct {
	ctx      context.Context
	remote   *net.UDPAddr
	queue    chan *mempool.Buffer
	conn     net.PacketConn
	proberCh <-chan *mempool.Buffer

	startOnce sync.Once
}

var _ conn.ConnWriter = (*udpSender)(nil)

func newUDPSender(ctx context.Context, probeCh <-chan *mempool.Buffer) *udpSender {
	return &udpSender{ctx: ctx, queue: make(chan *mempool.Buffer, 1024), proberCh: probeCh}
}

func (u *udpSender) waitInPacket(bufs *[][]byte, pendingBuf *[]*mempool.Buffer) error {
	appendPacket := func(pkt *mempool.Buffer, packetType protocol.PacketType) {
		protocol.MakeHeader(pkt, packetType)

		pkt.OffsetTo(0)

		fmt.Println("sent", pkt.Bytes())
		*bufs = append(*bufs, pkt.Bytes())
		*pendingBuf = append(*pendingBuf, pkt)
	}

	select {
	case pkt := <-u.proberCh:
		appendPacket(pkt, protocol.HeartBeat)
	case pkt := <-u.queue:
		appendPacket(pkt, protocol.TunEncap)
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for len(*bufs) < 1024 {
		select {
		case pkt := <-u.proberCh:
			appendPacket(pkt, protocol.HeartBeat)
		case pkt := <-u.queue:
			appendPacket(pkt, protocol.TunEncap)
		case <-u.ctx.Done():
			return u.ctx.Err()
		default:
			return nil
		}
	}

	return nil
}

func (u *udpSender) writeLoop() {
	bufs := make([][]byte, 0, 1024)
	pb := make([]*mempool.Buffer, 0, 1024)

	batchWriter := udp.NewWriterV4(u.conn, u.remote)

	for {
		err := u.waitInPacket(&bufs, &pb)
		if err != nil {
			break
		}
		_, err = batchWriter.WriteBatch(bufs)

		for _, b := range pb {
			mempool.Put(b)
		}
		pb = pb[:0]
		bufs = bufs[:0]

		if err != nil {
			fmt.Println(u.remote.String(), err)
			break
		}
	}
}

func (u *udpSender) Start(conn net.PacketConn, remote *net.UDPAddr) {
	u.startOnce.Do(func() {
		u.conn = conn
		u.remote = remote
		go u.writeLoop()
	})
}

func (u *udpSender) String() string {
	return u.remote.String()
}

func (u *udpSender) Write(b *mempool.Buffer) error {
	select {
	case <-u.ctx.Done():
		return u.ctx.Err()
	default:
		u.queue <- b
		return nil
	}
}
