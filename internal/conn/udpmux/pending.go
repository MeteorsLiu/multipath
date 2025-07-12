package udpmux

import (
	"bytes"

	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type pendingPacket struct {
	buf        *mempool.Buffer
	recvBuf    *bytes.Buffer
	expectSize int64
}

func newPendingPacket(buf []byte, expect int64) *pendingPacket {
	newBuf := mempool.Get(int(expect))
	recvBuf := bytes.NewBuffer(newBuf.B)
	recvBuf.Write(buf)
	return &pendingPacket{buf: newBuf, recvBuf: recvBuf, expectSize: expect}
}

func (p *pendingPacket) IsDone() bool {
	return p.recvBuf.Len() == int(p.expectSize)
}

func (p *pendingPacket) Write(buf []byte) (n int, done bool) {
	if p.IsDone() {
		done = true
		return
	}
	// don't resize
	size := min(len(buf), p.recvBuf.Available())

	n, _ = p.recvBuf.Write(buf[0:size])

	done = p.IsDone()

	return
}

func (p *pendingPacket) Release() {
	p.recvBuf = nil
	mempool.Put(p.buf)
}
