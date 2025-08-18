package tun

import (
	"context"
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/ip"
	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type TunHandler struct {
	ctx       context.Context
	osTun     conn.BatchConn
	inCh      chan *mempool.Buffer
	outWriter mempool.Writer
}

func NewHandler(ctx context.Context, tunInterface conn.BatchConn, outWriter mempool.Writer) *TunHandler {
	t := &TunHandler{ctx: ctx, osTun: tunInterface, inCh: make(chan *mempool.Buffer, 1024), outWriter: outWriter}
	go t.writeLoop()
	go t.readLoop()
	return t
}

func (t *TunHandler) In() chan<- *mempool.Buffer {
	return t.inCh
}

func (u *TunHandler) waitInPacket(bufs *[][]byte, pendingBuf *[]*mempool.Buffer) error {
	select {
	case pkt := <-u.inCh:
		*bufs = append(*bufs, pkt.Bytes())
		*pendingBuf = append(*pendingBuf, pkt)
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for len(*bufs) < 1024 {
		select {
		case pkt := <-u.inCh:
			*bufs = append(*bufs, pkt.Bytes())
			*pendingBuf = append(*pendingBuf, pkt)
		case <-u.ctx.Done():
			return u.ctx.Err()
		default:
			return nil
		}
	}

	return nil
}

func (u *TunHandler) writeLoop() {
	bufs := make([][]byte, 0, 1024)
	pb := make([]*mempool.Buffer, 0, 1024)
	for {
		err := u.waitInPacket(&bufs, &pb)
		if err != nil {
			return
		}
		_, err = u.osTun.WriteBatch(bufs)

		for _, b := range pb {
			mempool.Put(b)
		}
		pb = pb[:0]
		bufs = bufs[:0]

		if err != nil {
			break
		}
	}
}

func (u *TunHandler) readLoop() {
	bufs := make([]*mempool.Buffer, 1024)
	for i := range bufs {
		bufs[i] = mempool.Get(1500)
	}
	bufBytes := make([][]byte, 0, 1024)

	for _, b := range bufs {
		bufBytes = append(bufBytes, b.Bytes())
	}

	for {
		numMsgs, _, err := u.osTun.ReadBatch(bufBytes)
		if err != nil {
			break
		}
		for i := 0; i < numMsgs; i++ {
			size, err := ip.Header(bufs[i].Bytes()).Size()
			if err != nil {
				fmt.Println(err)
				continue
			}
			bufs[i].SetLen(int(size))
			u.outWriter.Write(bufs[i])
			bufs[i] = mempool.Get(1500)
		}
	}
}
