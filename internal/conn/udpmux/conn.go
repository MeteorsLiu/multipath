package udpmux

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

type udpConn struct {
	ctx          context.Context
	receiver     *udpReader
	isServerSide bool

	proberManager *prober.Manager

	manager *conn.SenderManager
}

func mustListenUDP(addr string) net.PacketConn {
	for i := 0; i < 100; i++ {
		udpC, err := net.ListenPacket("udp", addr)
		if err == nil {
			return udpC
		}
		sec := min(1<<i, 600)
		fmt.Printf("try to listen udp: %s fail: %v and wait for %d seconds\n", addr, err, sec)
		time.Sleep(time.Duration(sec) * time.Second)
	}
	panic("try listening udp too many times")
}

func DialConn(ctx context.Context, pm *conn.SenderManager, remoteAddr string, out chan<- *mempool.Buffer) {
	remoteUdpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		panic(err)
	}
	udpC := mustListenUDP(":0")

	cn := &udpConn{ctx: ctx, manager: pm, proberManager: prober.NewManager()}

	id, prober := cn.proberManager.Register(ctx, fmt.Sprintf("%s => %s", udpC.LocalAddr(), remoteAddr), cn.onProberEvent)

	sender := newUDPSender(ctx)
	cn.receiver = newUDPReceiver(ctx, udpC, out, sender.queue, cn.manager, cn.proberManager, cn.onRecvAddr, false)

	pm.Add(remoteAddr, func() (w conn.ConnWriter, onRemove func()) {
		return sender, func() {
			sender.Close()
			cn.receiver.Close()
			// start to dial a new one
			DialConn(ctx, pm, remoteAddr, out)
		}
	})

	cn.receiver.Start()

	sender.Start(udpC, remoteUdpAddr, prober.Out())
	prober.Start(sender, id)
}

func ListenConn(ctx context.Context, pm *conn.SenderManager, local string, out chan<- *mempool.Buffer) {
	localConn := mustListenUDP(local)
	conn := &udpConn{ctx: ctx, manager: pm, isServerSide: true, proberManager: prober.NewManager()}

	conn.receiver = newUDPReceiver(ctx, localConn, out, nil, conn.manager, conn.proberManager, conn.onRecvAddr, true)
	conn.receiver.Start()
}

func (c *udpConn) onProberEvent(sender conn.ConnWriter, event prober.Event) {
	switch event {
	case prober.Lost:
		c.manager.Remove(sender)
	}
}
func (c *udpConn) onRecvAddr(addr string) {
	if !c.isServerSide {
		return
	}
	c.manager.Add(addr, func() (w conn.ConnWriter, onRemove func()) {
		remoteAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			panic(err)
		}
		localC := mustListenUDP(":0")

		sender := newUDPSender(c.ctx)

		id, prober := c.proberManager.Register(c.ctx, fmt.Sprintf("%s => %s", localC.LocalAddr(), addr), c.onProberEvent)

		sender.Start(localC, remoteAddr, prober.Out())
		prober.Start(sender, id)

		fmt.Println("recv addr:", addr)
		return sender, func() {
			sender.Close()
		}
	})
}
