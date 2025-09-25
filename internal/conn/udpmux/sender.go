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
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/prometheus/client_golang/prometheus"
)

type udpSender struct {
	ctx        context.Context
	cancel     context.CancelFunc
	remoteAddr string
	queue      chan *mempool.Buffer
	conn       net.PacketConn
	proberOut  <-chan *mempool.Buffer
	startOnce  sync.Once
}

var _ conn.ConnWriter = (*udpSender)(nil)

func newUDPSender(ctx context.Context, cancel context.CancelFunc) *udpSender {
	sender := &udpSender{ctx: ctx, cancel: cancel, queue: make(chan *mempool.Buffer, 1024)}
	return sender
}

func (u *udpSender) waitInPacket(udpWriter *udp.SendMmsg, pendingBuf *[]*mempool.Buffer) error {
	appendPacket := func(pkt *mempool.Buffer, packetType protocol.PacketType) {
		if pkt.FullBytes()[0] == 0 {
			protocol.MakeHeader(pkt, packetType)
		}
		udpWriter.Write(pkt.FullBytes())
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

	var n int64
	remote, err := net.ResolveUDPAddr("udp", u.remoteAddr)
	if err != nil {
		panic(err)
	}
	batchWriter := udp.NewWriterV4(u.conn, remote)
	host, _, _ := net.SplitHostPort(u.remoteAddr)

	defer u.Close()

	for {
		err := u.waitInPacket(batchWriter, &pb)
		if err != nil {
			break
		}
		n, err = batchWriter.Submit()
		if n > 0 {
			prom.UDPTraffic.With(prometheus.Labels{"addr": host}).Add(float64(n))
		}

		for _, b := range pb {
			mempool.Put(b)
		}
		pb = pb[:0]

		if err != nil {
			fmt.Println("udp error: ", u.remoteAddr, err)
			break
		}
	}
}

func (u *udpSender) Start(conn net.PacketConn, remoteAddr string, proberOut <-chan *mempool.Buffer) {
	u.startOnce.Do(func() {
		u.conn = conn
		u.remoteAddr = remoteAddr
		u.proberOut = proberOut
		go u.writeLoop()
	})
}

func (u *udpSender) String() string {
	return u.remoteAddr
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
