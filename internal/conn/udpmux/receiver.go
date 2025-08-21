package udpmux

import (
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/ip"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

type pending struct {
	buf        *mempool.Buffer
	expectSize int
}

func newPending() *pending {
	return &pending{}
}

func (p *pending) Set(buf *mempool.Buffer, expect int) {
	p.buf = buf
	p.expectSize = expect
}

func (p *pending) Reset() {
	// cut off GC tracking
	p.buf = nil
}

func (p *pending) HasData() bool {
	return p.buf.Len() > 0
}

func (p *pending) Write(buf *mempool.Buffer) bool {
	nc, _ := p.buf.Write(buf.Bytes())

	buf.Consume(nc)

	return p.buf.ConsumedBytes() >= p.expectSize
}

func (p *pending) Buffer() *mempool.Buffer {
	// skip header
	p.buf.OffsetTo(protocol.HeaderSize)
	return p.buf
}

type udpReader struct {
	conn       net.PacketConn
	outCh      chan<- *mempool.Buffer
	proberIn   chan<- *mempool.Buffer
	onRecvAddr func(string)

	pending *pending

	startOnce sync.Once
}

func newUDPReceiver(
	conn net.PacketConn,
	outCh chan<- *mempool.Buffer,
	proberIn chan<- *mempool.Buffer,
	onRecvAddr func(string),
) *udpReader {
	return &udpReader{
		conn:       conn,
		proberIn:   proberIn,
		onRecvAddr: onRecvAddr,
		outCh:      outCh,
		pending:    newPending(),
	}
}

func (u *udpReader) handlePacket(buf *mempool.Buffer) error {
	if u.pending.HasData() {
		if u.pending.Write(buf) {
			u.outCh <- u.pending.Buffer()
			u.pending.Reset()
		}
		if buf.Len() == 0 {
			mempool.Put(buf)
			return nil
		}
	}
	// TODO: allow different protocol
	headerBuf := buf.Peek(protocol.HeaderSize)
	header := protocol.Header(headerBuf)

	payload := buf.Bytes()

	switch header.Type() {
	case protocol.HeartBeat:
		buf.SetLen(prober.NonceSize)
	case protocol.TunEncap:
		payloadSize, err := ip.Header(payload).Size()
		if err != nil {
			return nil
		}
		size := buf.Len()
		fullSize := int(payloadSize)

		if size < fullSize {
			buf.GrowTo(fullSize + protocol.HeaderSize)
			buf.Consume(size)
			u.pending.Set(buf, fullSize)
			return nil
		}
	}

	u.outCh <- buf

	return nil
}

func (u *udpReader) readLoop() {
	bufs := make([]*mempool.Buffer, 1024)
	for i := range bufs {
		bufs[i] = mempool.Get(1500)
	}
	bufBytes := make([][]byte, 0, 1024)

	batchReader := udp.NewReaderV4(u.conn)

	for {
		for _, b := range bufs {
			bufBytes = append(bufBytes, b.Bytes())
		}

		numMsgs, _, err := batchReader.ReadBatch(bufBytes)
		fmt.Println("recv", numMsgs, err)
		if err != nil {
			break
		}

		for i := 0; i < numMsgs; i++ {
			msg := batchReader.MessageAt(i)

			bufs[i].SetLen(msg.N)
			u.handlePacket(bufs[i])

			u.onRecvAddr(msg.Addr.String())
			// buffers in queue will be put back into the pool after consumed.
			// so we can grab a new buffer here
			bufs[i] = mempool.Get(1500)
		}
		// avoid memory leak
		bufBytes = bufBytes[:0]
	}
}

func (u *udpReader) Start() {
	u.startOnce.Do(func() {
		fmt.Println("start listening at ", u.conn.LocalAddr())
		go u.readLoop()
	})
}
