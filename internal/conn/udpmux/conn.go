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
	cancel       context.CancelFunc
	receiver     *udpReader
	isServerSide bool

	proberManager *prober.Manager

	manager *conn.SenderManager
}

type proberContext struct {
	addr   string
	sender *udpSender
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

	cn := &udpConn{manager: pm, proberManager: prober.NewManager()}
	cn.ctx, cn.cancel = context.WithCancel(ctx)

	id, prober := cn.proberManager.Register(cn.ctx, fmt.Sprintf("%s => %s", udpC.LocalAddr(), remoteAddr), cn.onProberEvent)

	sender := newUDPSender(cn.ctx, cn.cancel)
	cn.receiver = newUDPReceiver(cn.ctx, cn.cancel, udpC, out, sender.queue, cn.manager, cn.proberManager, cn.onRecvAddr, false)

	cn.receiver.Start()

	sender.Start(udpC, remoteUdpAddr, prober.Out())
	prober.Start(&proberContext{
		addr:   remoteAddr,
		sender: sender,
	}, id)
}

func ListenConn(ctx context.Context, pm *conn.SenderManager, local string, out chan<- *mempool.Buffer) {
	localConn := mustListenUDP(local)
	conn := &udpConn{manager: pm, isServerSide: true, proberManager: prober.NewManager()}
	conn.ctx, conn.cancel = context.WithCancel(ctx)

	conn.receiver = newUDPReceiver(conn.ctx, conn.cancel, localConn, out, nil, conn.manager, conn.proberManager, conn.onRecvAddr, true)
	conn.receiver.Start()
}

func (c *udpConn) onProberEvent(context any, event prober.Event) {
	proberContext := context.(*proberContext)
	switch event {
	case prober.Lost:
		c.manager.Remove(proberContext.sender)
	case prober.Normal:
		if c.isServerSide {
			return
		}
		c.manager.Add(proberContext.addr, func() (w conn.ConnWriter, onRemove func()) {
			return proberContext.sender, func() {
				proberContext.sender.Close()
				c.receiver.Close()
				// start to dial a new one
				DialConn(c.ctx, c.manager, proberContext.addr, c.receiver.outCh)
			}
		})
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

		ctx, cancel := context.WithCancel(c.ctx)

		sender := newUDPSender(ctx, cancel)

		id, prober := c.proberManager.Register(ctx, fmt.Sprintf("%s => %s", localC.LocalAddr(), addr), c.onProberEvent)

		sender.Start(localC, remoteAddr, prober.Out())
		prober.Start(&proberContext{
			addr:   addr,
			sender: sender,
		}, id)

		fmt.Println("recv addr:", addr)
		return sender, func() {
			sender.Close()
		}
	})
}
