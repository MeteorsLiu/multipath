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
	*udpSender

	ctx      context.Context
	cancel   context.CancelFunc
	receiver *udpReader

	proberManager *prober.Manager
}

func DialConn(ctx context.Context, listenAddr, remoteAddr string, out chan<- *mempool.Buffer) (conn.ConnWriter, error) {
	remoteUdpAddr, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		return nil, err
	}
	listenConn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return nil, err
	}
	remoteConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}

	cn := &udpConn{proberManager: prober.NewManager()}
	cn.ctx, cn.cancel = context.WithCancel(ctx)

	id, prober := cn.proberManager.Register(ctx, fmt.Sprintf("%s => %s", remoteConn.LocalAddr(), remoteAddr), cn.onProberEvent)

	cn.udpSender = newUDPSender(cn.ctx, prober)
	cn.receiver = newUDPReceiver(listenConn, out, cn.udpSender.queue, cn.proberManager, cn.onRecvAddr, false)

	cn.receiver.Start()
	cn.udpSender.Start(remoteConn, remoteUdpAddr)
	prober.Start(id)

	return cn, nil
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
	fmt.Println("recv", addr)

}
