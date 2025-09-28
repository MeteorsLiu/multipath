package tcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch"
	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/conn/protocol/ip"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/prober"
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/prometheus/client_golang/prometheus"
)

// max tcp mem: 64MB
const queueSize = 64*1024*1024/1500 + 1

type TcpConn struct {
	ctx           context.Context
	cancel        context.CancelFunc
	prober        *prober.Prober
	out           chan<- *mempool.Buffer
	queue         chan *mempool.Buffer
	manager       *path.PathManager
	proberManager *prober.Manager

	conn      net.Conn
	closeOnce sync.Once

	isServerSide bool
}

func mustDial(remoteAddr string) net.Conn {
	for i := 0; i < 100; i++ {
		dialer := &net.Dialer{
			Timeout: 15 * time.Second,
		}
		cn, err := dialer.Dial("tcp", remoteAddr)
		if err == nil {
			return cn
		}
		sec := min(1<<i, 600)
		fmt.Printf("try to listen udp: %s fail: %v and wait for %d seconds\n", remoteAddr, err, sec)
		time.Sleep(time.Duration(sec) * time.Second)
	}
	panic("try dialing tcp too many times")
}

func mustListen(addr string) net.Listener {
	for i := 0; i < 100; i++ {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			return l
		}
		sec := min(1<<i, 600)
		fmt.Printf("try to listen udp: %s fail: %v and wait for %d seconds\n", addr, err, sec)
		time.Sleep(time.Duration(sec) * time.Second)
	}
	panic("try dialing tcp too many times")
}

func (c *TcpConn) onProberEvent(context any, event prober.Event) {
	switch event {
	case prober.Lost:
		c.manager.Remove(c)
	}
}

func NewConn(ctx context.Context, pm *path.PathManager, cn net.Conn, out chan<- *mempool.Buffer, isServerSide bool) *TcpConn {
	tc := &TcpConn{
		conn: cn, manager: pm, isServerSide: isServerSide,
		queue: make(chan *mempool.Buffer, queueSize),
		out:   out, proberManager: prober.NewManager(),
	}
	tc.ctx, tc.cancel = context.WithCancel(ctx)

	tc.Start()

	pm.Add(tc.String(), func() (w conn.ConnWriter, onRemove func()) {
		return tc, func() {
			if isServerSide {
				return
			}
			DialConn(ctx, pm, cn.RemoteAddr().String(), out)
		}
	})
	return tc
}

func DialConn(ctx context.Context, pm *path.PathManager, remoteAddr string, out chan<- *mempool.Buffer) {
	cn := mustDial(remoteAddr)
	NewConn(ctx, pm, cn, out, false)
}

func ListenConn(ctx context.Context, pm *path.PathManager, local string, out chan<- *mempool.Buffer) {
	listener := mustListen(local)

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				continue
			}
			NewConn(ctx, pm, c, out, true)
		}
	}()
}

func (t *TcpConn) Start() {
	id, prober := t.proberManager.Register(t.ctx, fmt.Sprintf("%s => %s", t.conn.LocalAddr(), t.conn.RemoteAddr()), t.onProberEvent)
	t.prober = prober
	go t.readLoop()
	go t.writeLoop()
	t.prober.Start(nil, id)
}

func (t *TcpConn) Close() error {
	t.cancel()
	return t.conn.Close()
}

func (u *TcpConn) readLoop() {
	b := make([]byte, protocol.HeaderSize)
	reader := bufio.NewReader(u.conn)
	defer u.Close()
	var err error
	const proberPacketSize = prober.NonceSize + prober.ProbeHeaderSize
	fmt.Printf("read tcp at: %s => %s\n", u.conn.LocalAddr(), u.conn.RemoteAddr())
	var n int64
	host, _, _ := net.SplitHostPort(u.conn.RemoteAddr().String())

loop:
	for {
		_, err = reader.Read(b)
		if err != nil {
			break
		}
		header := protocol.Header(b)

		switch header.Type() {
		case protocol.HeartBeat:
			// we can't read packets like UDP, because TCP is stream-origent
			buf := mempool.GetWithHeader(proberPacketSize, protocol.HeaderSize)
			if _, err = io.ReadFull(reader, buf.Bytes()); err != nil {
				break loop
			}
			err = u.proberManager.PacketIn(u.ctx, buf)
			if err == prober.ErrProberIDNotFound {
				// sent back
				buf.WriteAt(b, 0)
				u.queue <- buf
				continue loop
			}
			if err != nil {
				mempool.Put(buf)
				fmt.Println(err)
			}
		case protocol.TunEncap:
			buf := mempool.Get(1500)
			// it's ok to read again here, because bufio can reduce the time of calling syscall
			if _, err = io.ReadFull(reader, buf.Bytes()[:20]); err != nil {
				break loop
			}
			var packetSize uint16
			packetSize, err = ip.Header(buf.Bytes()).Size()
			if err != nil {
				mempool.Put(buf)
				continue loop
			}
			// it looks like an jumbo frame, drop it
			if packetSize > 1500 || packetSize <= 20 {
				mempool.Put(buf)
				continue loop
			}
			if n > 0 {
				prom.TCPTraffic.With(prometheus.Labels{"addr": host}).Add(float64(packetSize))
			}
			buf.SetLen(int(packetSize))

			if _, err = io.ReadFull(reader, buf.Bytes()[20:packetSize]); err != nil {
				break loop
			}
			u.out <- buf
		}
	}
	if err != nil {
		fmt.Println("readloop exits: ", err)
	}
}

func (u *TcpConn) waitInPacket(tcpWriter conn.BatchWriter, pendingBuf *[]*mempool.Buffer) error {
	appendPacket := func(pkt *mempool.Buffer, packetType protocol.PacketType) {
		if pkt.FullBytes()[0] == 0 {
			protocol.MakeHeader(pkt, packetType)
		}
		tcpWriter.Write(pkt.FullBytes())
		*pendingBuf = append(*pendingBuf, pkt)
	}

	select {
	case pkt := <-u.prober.Out():
		appendPacket(pkt, protocol.HeartBeat)
	case pkt := <-u.queue:
		appendPacket(pkt, protocol.TunEncap)
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for len(*pendingBuf) < 1024 {
		select {
		case pkt := <-u.prober.Out():
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

func (t *TcpConn) writeLoop() {
	cn := t.conn.(syscall.Conn)
	rw, err := cn.SyscallConn()
	if err != nil {
		panic(err)
	}
	defer t.Close()

	batchWriter := batch.NewWriter(rw)
	pb := make([]*mempool.Buffer, 0, 1024)
	fmt.Printf("write tcp at: %s => %s\n", t.conn.LocalAddr(), t.conn.RemoteAddr())
	var n int64
	host, _, _ := net.SplitHostPort(t.conn.RemoteAddr().String())

	for {
		err = t.waitInPacket(batchWriter, &pb)
		if err != nil {
			break
		}
		n, err = batchWriter.Submit()
		if n > 0 {
			prom.TCPTraffic.With(prometheus.Labels{"addr": host}).Add(float64(n))
		}

		for _, b := range pb {
			mempool.Put(b)
		}
		pb = pb[:0]

		if err != nil {
			break
		}
	}
	if err != nil {
		fmt.Println("writeloop exits", err)
	}
}

func (t *TcpConn) String() string {
	return fmt.Sprintf("%s => %s", t.conn.LocalAddr(), t.conn.RemoteAddr())
}

func (t *TcpConn) Prober() *prober.Prober {
	return t.prober
}

func (t *TcpConn) Write(b *mempool.Buffer) error {
	select {
	case <-t.ctx.Done():
		return t.ctx.Err()
	case t.queue <- b:
		return nil
	}
}

func (u *TcpConn) Remote() string {
	return u.conn.RemoteAddr().String()
}
