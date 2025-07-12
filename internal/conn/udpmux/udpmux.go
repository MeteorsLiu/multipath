package udpmux

import (
	"context"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn/batch/udp"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prober"
)

type UDPMuxer struct {
	ctx  context.Context
	conn net.PacketConn
	// 1:1 binding
	startOnce     sync.Once
	prober        *prober.Prober
	packetIn      chan *[]byte
	packetOut     chan<- *mempool.Buffer
	pendingPacket *mempool.Buffer
}

func NewClient() {

}

func (u *UDPMuxer) Start() {
	u.startOnce.Do(func() {

	})
}

func (u *UDPMuxer) writeLoop() {
	bufs := make([][]byte, 0, 1024)

	const headerSize = 1024 * protocol.MaxHeaderSize

	headerBuf := make([]byte, headerSize)

	batchWriter := udp.NewWriterV4(u.conn)

	for {
		err := u.waitInPacket(&bufs, headerBuf)
		if err != nil {
			return
		}
		_, err = batchWriter.WriteBatch(bufs)
		if err != nil {
			return
		}

		bufs = bufs[:0]
	}
}

func (u *UDPMuxer) readLoop() {
	bufs := make([][]byte, 1024)

	batchReader := udp.NewReaderV4(u.conn)

	for {
		batchReader.ReadBatch(bufs)
	}
}

func (u *UDPMuxer) waitInPacket(bufs *[][]byte, headerBuf []byte) error {
	appendEncap := func(pkt *[]byte) {
		headerSize := protocol.MakeEncapHeader(headerBuf, len(*pkt))

		header := headerBuf[0:headerSize]

		*bufs = append(*bufs, header)
		*bufs = append(*bufs, *pkt)

		headerBuf = headerBuf[headerSize:]
	}

	appendHeartBeat := func(pkt *prober.Packet) {
		headerSize := protocol.MakeHeartBeatHeader(headerBuf)

		header := headerBuf[0:headerSize]

		*bufs = append(*bufs, header)
		*bufs = append(*bufs, pkt.Nonce)

		headerBuf = headerBuf[headerSize:]
	}

	select {
	case pkt := <-u.prober.Out():
		appendHeartBeat(pkt)
	case pkt := <-u.packetIn:
		appendEncap(pkt)
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for len(*bufs) < 1024-2 {
		select {
		case pkt := <-u.prober.Out():
			appendHeartBeat(pkt)
		case pkt := <-u.packetIn:
			appendEncap(pkt)
		case <-u.ctx.Done():
			return u.ctx.Err()
		default:
			return nil
		}
	}

	return nil
}
