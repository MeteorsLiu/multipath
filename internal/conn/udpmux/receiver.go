package udpmux

import (
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/ip"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

type pending struct {
	buf        *mempool.Buffer
	pktType    protocol.PacketType
	expectSize int
}

func newPending() *pending {
	return &pending{}
}

func (p *pending) Set(buf *mempool.Buffer, expect int, pktType protocol.PacketType) {
	p.buf = buf
	p.pktType = pktType
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

func (p *pending) Buffer() (buf *mempool.Buffer, pktType protocol.PacketType) {
	// skip header
	p.buf.OffsetTo(protocol.HeaderSize)
	return p.buf, pktType
}

type udpReader struct {
	conn  net.PacketConn
	outCh chan<- *mempool.Buffer

	clientSender  chan<- *mempool.Buffer
	senderManager *conn.SenderManager
	proberManager *prober.Manager
	onRecvAddr    func(string)

	pending *pending

	startOnce    sync.Once
	isServerSide bool
}

func newUDPReceiver(
	conn net.PacketConn,
	outCh chan<- *mempool.Buffer,
	clientSender chan<- *mempool.Buffer,
	senderManager *conn.SenderManager,
	proberManager *prober.Manager,
	onRecvAddr func(string),
	isServerSide bool,
) *udpReader {
	return &udpReader{
		conn:          conn,
		senderManager: senderManager,
		proberManager: proberManager,
		onRecvAddr:    onRecvAddr,
		isServerSide:  isServerSide,
		outCh:         outCh,
		clientSender:  clientSender,
		pending:       newPending(),
	}
}

func (u *udpReader) sendPacketToRemote(addr string, pkt *mempool.Buffer) {
	if !u.isServerSide {
		u.clientSender <- pkt
		return
	}
	sender := u.senderManager.Get(addr)
	if sender == nil {
		panic("sender is nil")
	}
	if err := sender.Write(pkt); err != nil {
		fmt.Println(err)
	}
}

func (u *udpReader) recvProbe(addr string, pkt *mempool.Buffer) {
	err := u.proberManager.PacketIn(pkt)
	if err == prober.ErrProberIDNotFound {
		u.sendPacketToRemote(addr, pkt)
		return
	}
	if err != nil {
		fmt.Println("probe err: ", err)
		mempool.Put(pkt)
		return
	}
}

func (u *udpReader) handlePacket(addr string, buf *mempool.Buffer) error {
	if u.pending.HasData() {
		done := u.pending.Write(buf)

		if done {
			pendingBuf, pktType := u.pending.Buffer()

			switch pktType {
			case protocol.HeartBeat:
				u.recvProbe(addr, pendingBuf)
			case protocol.TunEncap:
				u.outCh <- pendingBuf
			}
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
		if len(payload) < prober.NonceSize {
			buf.GrowTo(prober.NonceSize + protocol.HeaderSize)
			u.pending.Set(buf, prober.NonceSize, protocol.HeartBeat)
			return nil
		}
		u.recvProbe(addr, buf)
	case protocol.TunEncap:
		size := len(payload)
		if size > 1500 || size <= 20 {
			fmt.Println("small size: drop ", size)
			mempool.Put(buf)
			return nil
		}
		payloadSize, err := ip.Header(payload).Size()
		if err != nil {
			fmt.Println("small size: ", err, buf.FullBytes())

			return nil
		}
		fullSize := int(payloadSize)

		if size < fullSize {
			buf.GrowTo(fullSize + protocol.HeaderSize)
			buf.Consume(size)
			u.pending.Set(buf, fullSize, protocol.TunEncap)
			return nil
		}
		u.outCh <- buf
	}
	return nil
}

func (u *udpReader) readLoop() {
	batchReader := udp.NewReaderV4(u.conn)
	bufs := make([]*mempool.Buffer, 1024)
	for i := range bufs {
		bufs[i] = mempool.Get(1500)
		msg := batchReader.MessageAt(i)
		msg.Buffers[0] = bufs[i].FullBytes()
	}

	trafficMap := make(map[string]int64)

	for {
		numMsgs, _, err := batchReader.ReadMessage()
		if err != nil {
			fmt.Println("udp read error: ", err)
			break
		}

		for i := 0; i < numMsgs; i++ {
			msg := batchReader.MessageAt(i)

			bufs[i].SetLen(msg.N)

			remoteAddr := msg.Addr.String()
			u.onRecvAddr(remoteAddr)

			u.handlePacket(remoteAddr, bufs[i])

			trafficMap[msg.Addr.String()] += int64(msg.N)

			// buffers in queue will be put back into the pool after consumed.
			// so we can grab a new buffer here
			bufs[i] = mempool.Get(1500)
			// update receive queue
			msg.Buffers[0] = bufs[i].FullBytes()
		}
	}
}

func (u *udpReader) Start() {
	u.startOnce.Do(func() {
		fmt.Println("start listening at ", u.conn.LocalAddr())
		go u.readLoop()
	})
}
