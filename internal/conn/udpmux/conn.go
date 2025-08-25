package udpmux

import (
	"context"
	"fmt"
	"net"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

type udpConn struct {
	ctx          context.Context
	cancel       context.CancelFunc
	receiver     *udpReader
	sender       *udpSender
	prober       *prober.Prober
	isServerSide bool

	manager *conn.SenderManager
}

func DialConn(ctx context.Context, pm *conn.SenderManager, remoteAddr string, out chan<- *mempool.Buffer) (conn.MuxConn, error) {
	remoteUdpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return nil, err
	}
	udpC, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}

	cn := &udpConn{manager: pm}
	cn.ctx, cn.cancel = context.WithCancel(ctx)
	cn.prober = prober.New(cn.ctx, cn.onProberEvent)

	cn.receiver = newUDPReceiver(udpC, out, cn.prober.In(), cn.onRecvAddr)
	cn.sender = newUDPSender(cn.ctx, cn.prober.Out())

	cn.receiver.Start()
	cn.sender.Start(udpC, remoteUdpAddr)
	cn.prober.Start()

	pm.Add(remoteAddr, func() conn.ConnWriter {
		return cn.sender
	})

	return cn, nil
}

func ListenConn(ctx context.Context, pm *conn.SenderManager, local string, out chan<- *mempool.Buffer) (conn.MuxConn, error) {
	localConn, err := net.ListenPacket("udp", local)
	if err != nil {
		return nil, err
	}
	conn := &udpConn{manager: pm, isServerSide: true}
	conn.ctx, conn.cancel = context.WithCancel(ctx)
	conn.prober = prober.New(conn.ctx, conn.onProberEvent)

	conn.receiver = newUDPReceiver(localConn, out, conn.prober.In(), conn.onRecvAddr)
	conn.sender = newUDPSender(conn.ctx, conn.prober.Out())

	conn.receiver.Start()

	return conn, nil
}

func (c *udpConn) onProberEvent(event prober.Event) {
	// switch event {
	// case prober.Disconnected:
	// 	c.manager.Remove(c.sender)
	// case prober.Normal:
	// 	c.manager.Add(c.sender.String(), func() conn.ConnWriter {
	// 		return c.sender
	// 	})
	// }
}
func (c *udpConn) onRecvAddr(addr string) {
	if !c.isServerSide {
		return
	}
	c.manager.Add(addr, func() conn.ConnWriter {
		remoteAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			fmt.Println("failed to parse udp addr when onRecvAddr: ", err)
			return nil
		}
		localC, err := net.ListenPacket("udp", ":0")
		if err != nil {
			fmt.Println("failed to listen udp when onRecvAddr: ", err)
			return nil
		}
		c.sender.Start(localC, remoteAddr)
		fmt.Println("on addr :", addr)

		return c.sender
	})
}
