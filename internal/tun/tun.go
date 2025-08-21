package tun

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

type OSTun interface {
	io.Reader
	conn.BatchConn
}
type TunHandler struct {
	ctx       context.Context
	osTun     OSTun
	inCh      chan *mempool.Buffer
	outWriter mempool.Writer
}

func NewHandler(ctx context.Context, tunInterface OSTun, outWriter mempool.Writer) *TunHandler {
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
	for {
		buf := mempool.Get(1500)
		n, err := u.osTun.Read(buf.Bytes())
		if err != nil {
			fmt.Println("readloop exit: ", err)
			break
		}
		buf.SetLen(n)
		err = u.outWriter.Write(buf)

		if errors.Is(err, scheduler.ErrNoPath) {
			mempool.Put(buf)
			continue
		}
		if err != nil {
			fmt.Println("readloop exit: ", err)
			break
		}
	}
}
