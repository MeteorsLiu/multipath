package udpmux

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/ip"
	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

var errPacketConsumed = fmt.Errorf("packet need consumed")

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
	ctx    context.Context
	cancel context.CancelFunc
	conn   net.PacketConn
	outCh  chan<- *mempool.Buffer

	clientSender  chan<- *mempool.Buffer
	senderManager *conn.SenderManager
	proberManager *prober.Manager
	onRecvAddr    func(string)

	pending *pending

	startOnce    sync.Once
	isServerSide bool
}

func newUDPReceiver(
	ctx context.Context,
	conn net.PacketConn,
	outCh chan<- *mempool.Buffer,
	clientSender chan<- *mempool.Buffer,
	senderManager *conn.SenderManager,
	proberManager *prober.Manager,
	onRecvAddr func(string),
	isServerSide bool,
) *udpReader {
	reader := &udpReader{
		conn:          conn,
		senderManager: senderManager,
		proberManager: proberManager,
		onRecvAddr:    onRecvAddr,
		isServerSide:  isServerSide,
		outCh:         outCh,
		clientSender:  clientSender,
		pending:       newPending(),
	}
	reader.ctx, reader.cancel = context.WithCancel(ctx)
	return reader
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

func (u *udpReader) recvProbe(addr string, pkt *mempool.Buffer) error {
	fmt.Println("packet in: ", pkt.FullBytes())
	err := u.proberManager.PacketIn(u.ctx, pkt)
	if err == prober.ErrProberIDNotFound {
		u.sendPacketToRemote(addr, pkt)
		return nil
	}
	if err != nil {
		select {
		case <-u.ctx.Done():
			return u.ctx.Err()
		default:
			return errPacketConsumed
		}
	}
	return nil
}

func (u *udpReader) handlePacket(addr string, buf *mempool.Buffer) error {
	if u.pending.HasData() {
		done := u.pending.Write(buf)

		if done {
			pendingBuf, pktType := u.pending.Buffer()

			switch pktType {
			case protocol.HeartBeat:
				if err := u.recvProbe(addr, pendingBuf); err != nil {
					return err
				}
			case protocol.TunEncap:
				select {
				case u.outCh <- buf:
				case <-u.ctx.Done():
					return u.ctx.Err()
				}
			}
		}
		if buf.Len() == 0 {
			return errPacketConsumed
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
		if err := u.recvProbe(addr, buf); err != nil {
			return err
		}
	case protocol.TunEncap:
		size := len(payload)
		if size > 1500 || size <= 20 {
			return errPacketConsumed
		}
		payloadSize, err := ip.Header(payload).Size()
		if err != nil {
			return errPacketConsumed
		}
		fullSize := int(payloadSize)

		if size < fullSize {
			buf.GrowTo(fullSize + protocol.HeaderSize)
			buf.Consume(size)
			u.pending.Set(buf, fullSize, protocol.TunEncap)
			return nil
		}
		select {
		case u.outCh <- buf:
		case <-u.ctx.Done():
			return u.ctx.Err()
		}
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

	defer func() {
		// if someone closes us, we have to recycle our buffers
		for _, b := range bufs {
			mempool.Put(b)
		}
	}()

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

			err := u.handlePacket(remoteAddr, bufs[i])

			switch err {
			case nil:
			case errPacketConsumed:
				mempool.Put(bufs[i])
			default:
				return
			}

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

func (u *udpReader) Close() error {
	u.cancel()
	return u.conn.Close()
}
