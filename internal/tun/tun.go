package tun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

type OSTun interface {
	io.ReadWriter
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
	return t
}

func (t *TunHandler) Start() {
	go t.writeLoop()
	go t.readLoop()
}

func (t *TunHandler) In() chan<- *mempool.Buffer {
	return t.inCh
}

func (u *TunHandler) write(buf *mempool.Buffer) error {
	_, err := u.osTun.Write(buf.Bytes())
	mempool.Put(buf)
	return err
}

func (u *TunHandler) waitInPacket() error {
	select {
	case pkt := <-u.inCh:
		if err := u.write(pkt); err != nil {
			return err
		}
	case <-u.ctx.Done():
		return u.ctx.Err()
	}

	for {
		select {
		case pkt := <-u.inCh:
			if err := u.write(pkt); err != nil {
				return err
			}
		case <-u.ctx.Done():
			return u.ctx.Err()
		default:
			return nil
		}
	}
}

func (u *TunHandler) writeLoop() {
	for {
		err := u.waitInPacket()
		if err != nil && err != syscall.EIO {
			fmt.Println("write exits: ", err)
			return
		}
	}
}

func (u *TunHandler) readLoop() {
	for {
		buf := mempool.GetWithHeader(1500, protocol.HeaderSize)
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
