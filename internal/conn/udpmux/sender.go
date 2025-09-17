package udpmux

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type udpSender struct {
	ctx       context.Context
	cancel    context.CancelFunc
	remote    *net.UDPAddr
	queue     chan *mempool.Buffer
	conn      net.PacketConn
	proberOut <-chan *mempool.Buffer
	startOnce sync.Once
}

var _ conn.ConnWriter = (*udpSender)(nil)

func newUDPSender(ctx context.Context) *udpSender {
	sender := &udpSender{queue: make(chan *mempool.Buffer, 1024)}
	sender.ctx, sender.cancel = context.WithCancel(ctx)
	return sender
}

func (u *udpSender) waitInPacket(udpWriter *udp.SendMmsg, pendingBuf *[]*mempool.Buffer) error {
	appendPacket := func(pkt *mempool.Buffer, packetType protocol.PacketType) {
		if !pkt.IsHeaderInitialized() {
			protocol.MakeHeader(pkt, packetType)
		}
		udpWriter.Write(pkt)
		*pendingBuf = append(*pendingBuf, pkt)
	}

	select {
	case pkt := <-u.proberOut:
		appendPacket(pkt, protocol.HeartBeat)
	case pkt := <-u.queue:
		appendPacket(pkt, protocol.TunEncap)
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for len(*pendingBuf) < 1024 {
		select {
		case pkt := <-u.proberOut:
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
	pb := make([]*mempool.Buffer, 0, 1024)

	batchWriter := udp.NewWriterV4(u.conn, u.remote)

	for {
		err := u.waitInPacket(batchWriter, &pb)
		if err != nil {
			break
		}
		_, err = batchWriter.Submit()

		for _, b := range pb {
			mempool.Put(b)
		}
		pb = pb[:0]

		if err != nil {
			fmt.Println("udp error: ", u.remote.String(), err)
			break
		}
	}
}

func (u *udpSender) Start(conn net.PacketConn, remote *net.UDPAddr, proberOut <-chan *mempool.Buffer) {
	u.startOnce.Do(func() {
		u.conn = conn
		u.remote = remote
		u.proberOut = proberOut
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

func (u *udpSender) Close() error {
	u.cancel()
	return u.conn.Close()
}
