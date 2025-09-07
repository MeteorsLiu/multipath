package tcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch"
	"github.com/MeteorsLiu/multipath/internal/conn/ip"
	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

// max tcp mem: 64MB
const queueSize = 64*1024*1024/1500 + 1

type tcpConn struct {
	ctx           context.Context
	prober        *prober.Prober
	out           chan<- *mempool.Buffer
	queue         chan *mempool.Buffer
	proberManager *prober.Manager

	conn net.Conn
}

func (c *tcpConn) onProberEvent(event prober.Event) {
	// switch event {
	// case prober.Disconnected:
	// 	c.manager.Remove(c.sender)
	// case prober.Normal:
	// 	c.manager.Add(c.sender.String(), func() conn.ConnWriter {
	// 		return c.sender
	// 	})
	// }
}

func NewConn(ctx context.Context, cn net.Conn, out chan<- *mempool.Buffer) *tcpConn {
	tc := &tcpConn{ctx: ctx, conn: cn, queue: make(chan *mempool.Buffer, queueSize), out: out, proberManager: prober.NewManager()}
	tc.Start()
	id, prober := tc.proberManager.Register(ctx, fmt.Sprintf("%s => %s", cn.LocalAddr(), cn.RemoteAddr()), tc.onProberEvent)
	tc.prober = prober
	tc.prober.Start(id)
	return tc
}

func DialConn(ctx context.Context, remoteAddr string, out chan<- *mempool.Buffer) (conn.ConnWriter, error) {
	dialer := &net.Dialer{
		Timeout: 15 * time.Second,
	}
	cn, err := dialer.Dial("tcp", remoteAddr)
	if err != nil {
		return nil, err
	}
	return NewConn(ctx, cn, out), nil
}

func (t *tcpConn) Start() {
	go t.readLoop()
	go t.writeLoop()
}

func (u *tcpConn) readLoop() {
	b := make([]byte, protocol.HeaderSize)
	reader := bufio.NewReader(u.conn)
	var err error
	const proberPacketSize = prober.NonceSize + prober.ProbeHeaderSize
	fmt.Printf("read tcp at: %s => %s\n", u.conn.LocalAddr(), u.conn.RemoteAddr())
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
			err = u.proberManager.PacketIn(buf)
			if err == prober.ErrProberIDNotFound {
				// sent back
				buf.WriteAt(b, 0)
				u.queue <- buf
				continue
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
			if _, err = io.ReadFull(reader, buf.Bytes()[20:packetSize]); err != nil {
				break loop
			}
			fmt.Println(packetSize, buf)
			u.out <- buf
		}
	}
	if err != nil {
		fmt.Println("readloop exits: ", err)
	}
}

func (u *tcpConn) waitInPacket(tcpWriter conn.BatchWriter, pendingBuf *[]*mempool.Buffer) error {
	appendPacket := func(pkt *mempool.Buffer, packetType protocol.PacketType) {
		if !pkt.IsHeaderInitialized() {
			protocol.MakeHeader(pkt, packetType)
		}
		tcpWriter.Write(pkt)
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

	for len(*pendingBuf) < queueSize {
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

func (t *tcpConn) writeLoop() {
	cn := t.conn.(syscall.Conn)
	rw, err := cn.SyscallConn()
	if err != nil {
		panic(err)
	}
	batchWriter := batch.NewWriter(rw)
	pb := make([]*mempool.Buffer, 0, queueSize)
	fmt.Printf("write tcp at: %s => %s\n", t.conn.LocalAddr(), t.conn.RemoteAddr())

	for {
		err = t.waitInPacket(batchWriter, &pb)
		if err != nil {
			break
		}
		_, err = batchWriter.Submit()

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

func (t *tcpConn) String() string {
	return fmt.Sprintf("%s => %s", t.conn.LocalAddr(), t.conn.RemoteAddr())
}

func (t *tcpConn) Prober() *prober.Prober {
	return t.prober
}

func (t *tcpConn) Write(b *mempool.Buffer) error {
	select {
	case <-t.ctx.Done():
		return t.ctx.Err()
	case t.queue <- b:
		return nil
	}
}
