package packetbuf

import (
	"math/bits"
	"sync"
)

const (
	maxPooledSize = 64 * 1024
)

type Packet struct {
	Payload []byte

	buf   []byte
	class int
}

var packetPools [17]sync.Pool

func Acquire(size int) *Packet {
	class := sizeClass(size)
	var packet *Packet
	if class >= 0 {
		if pooled, ok := packetPools[class].Get().(*Packet); ok {
			packet = pooled
		}
	} else {
		packet = &Packet{class: -1}
	}
	if packet == nil {
		packet = &Packet{
			buf:   make([]byte, 1<<class),
			class: class,
		}
	}
	if cap(packet.buf) < size {
		packet.buf = make([]byte, size)
		packet.class = -1
	}
	packet.Payload = packet.buf[:size]
	return packet
}

func (p *Packet) SetLen(n int) {
	p.Payload = p.buf[:n:n]
}

func (p *Packet) Release() {
	if p == nil {
		return
	}
	p.Payload = nil
	if cap(p.buf) > 0 {
		p.buf = p.buf[:cap(p.buf)]
	}
	if p.class < 0 || p.class >= len(packetPools) || cap(p.buf) != 1<<p.class {
		return
	}
	packetPools[p.class].Put(p)
}

func sizeClass(size int) int {
	if size <= 0 {
		return 0
	}
	if size > maxPooledSize {
		return -1
	}
	return bits.Len(uint(size - 1))
}
