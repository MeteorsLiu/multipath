package mempool

// Inspired by https://github.com/xtaci/smux/blob/master/alloc.go

import (
	"errors"
	"math/bits"
	"sync"
)

var (
	defaultAllocator = NewAllocator()
)

type Buffer struct {
	B []byte
}

func ToBuffer(b []byte) *Buffer { return &Buffer{B: b} }

func (b *Buffer) Bytes() []byte {
	return b.B
}

func (b *Buffer) Reset() {
	clear(b.B)
}

func (b *Buffer) SetLen(len int) {
	b.B = b.B[:len]
}

func (b *Buffer) Cap() int {
	return cap(b.B)
}

func Get(size int) *Buffer {
	if defaultAllocator == nil {
		return nil
	}
	return defaultAllocator.Get(size)
}

func Put(buf *Buffer) error {
	if defaultAllocator == nil {
		return nil
	}
	return defaultAllocator.Put(buf)
}

// Allocator for incoming frames, optimized to prevent overwriting after zeroing
type Allocator struct {
	buffers []sync.Pool
}

// NewAllocator initiates a []byte allocator for frames less than 65536 bytes,
// the waste(memory fragmentation) of space allocation is guaranteed to be
// no more than 50%.
func NewAllocator() *Allocator {
	alloc := new(Allocator)
	alloc.buffers = make([]sync.Pool, 17) // 1B -> 64K
	for k := range alloc.buffers {
		i := k
		alloc.buffers[k].New = func() any {
			return &Buffer{B: make([]byte, 1<<uint32(i))}
		}
	}
	return alloc
}

// Get a []byte from pool with most appropriate cap
func (alloc *Allocator) Get(size int) *Buffer {
	if size <= 0 || size > 65536 {
		return nil
	}

	bits := msb(size)
	var b *Buffer
	if size == 1<<bits {
		b = alloc.buffers[bits].Get().(*Buffer)
	} else {
		b = alloc.buffers[bits+1].Get().(*Buffer)
	}
	b.SetLen(size)

	return b
}

// Put returns a []byte to pool for future use,
// which the cap must be exactly 2^n
func (alloc *Allocator) Put(b *Buffer) error {
	bits := msb(b.Cap())
	if b.Cap() == 0 || b.Cap() > 65536 || b.Cap() != 1<<bits {
		return errors.New("allocator Put() incorrect buffer size")
	}
	alloc.buffers[bits].Put(b)
	return nil
}

// msb return the pos of most significant bit
func msb(size int) uint16 {
	return uint16(bits.Len32(uint32(size)) - 1)
}
