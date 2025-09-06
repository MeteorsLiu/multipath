package mempool

// Inspired by https://github.com/xtaci/smux/blob/master/alloc.go

import (
	"errors"
	"io"
	"math/bits"
	"sync"
)

type Writer interface {
	Write(*Buffer) error
}

var (
	defaultAllocator = NewAllocator()
)

var _ io.ReaderFrom = (*Buffer)(nil)

type Buffer struct {
	b            []byte
	pos          int
	reservedSize int
}

var _ io.WriterAt = (*Buffer)(nil)

func ToBuffer(b []byte) *Buffer { return &Buffer{b: b} }

func (b *Buffer) offset() int {
	return b.pos + b.reservedSize
}

func (b *Buffer) Reserve(n int) {
	b.reservedSize = n
}

func (b *Buffer) WriteAt(p []byte, off int64) (n int, err error) {
	n = copy(b.b[off:], p)
	return
}

func (b *Buffer) WriteByteAt(p byte, off int64) (err error) {
	b.b[off] = p
	return
}

func (b *Buffer) Read(buf []byte) (n int, err error) {
	n = copy(buf, b.b[b.offset():])
	return
}

func (b *Buffer) Write(buf []byte) (n int, err error) {
	n = copy(b.b[b.offset():], buf)
	// move cursor by default
	b.Consume(n)
	return
}

func (b *Buffer) ReadFrom(r io.Reader) (n int64, err error) {
	var nr int
	nr, err = io.ReadFull(r, b.b[b.offset():])
	n = int64(nr)
	return
}

func (b *Buffer) Peek(n int) []byte {
	pos := b.offset()
	b.Consume(n)
	return b.b[pos : n+pos]
}

func (b *Buffer) FullBytes() []byte {
	return b.b
}

func (b *Buffer) Header() []byte {
	if b.reservedSize > 0 {
		return b.b[0:b.reservedSize]
	}
	return nil
}

func (b *Buffer) IsHeaderInitialized() bool {
	header := b.Header()
	return header != nil && header[0] != 0
}

func (b *Buffer) Bytes() []byte {
	return b.b[b.offset():]
}

func (b *Buffer) ShallowCopy(pos, headerSize int) *Buffer {
	return &Buffer{pos: pos, reservedSize: headerSize, b: b.b}
}

func (b *Buffer) OffsetTo(n int) {
	b.pos = n
}

func (b *Buffer) resetCursor() {
	b.pos = 0
	b.reservedSize = 0
	// allow GC
	b.SetLen(0)
}

func (b *Buffer) Reset() {
	clear(b.b[b.offset():])
}

func (b *Buffer) ResetFull() {
	clear(b.b)
}

func (b *Buffer) SetLen(len int) {
	b.b = b.b[:len+b.reservedSize]
}

func (b *Buffer) Cap() int {
	return cap(b.b)
}

func (b *Buffer) Len() int {
	if b == nil {
		return 0
	}
	return len(b.Bytes())
}

func (b *Buffer) Consume(n int) {
	b.pos = min(b.pos+n, len(b.b))
}

func (b *Buffer) GrowTo(n int) {
	if b.Cap() >= n {
		b.SetLen(n)
		return
	}
	newBuf := Get(n)
	copy(newBuf.b, b.b)

	Put(ToBuffer(b.b))

	b.b = newBuf.b
	newBuf.b = nil
}

func (b *Buffer) ConsumedBytes() int {
	return b.pos
}

func GetWithHeader(size, headerSize int) *Buffer {
	if defaultAllocator == nil {
		return nil
	}
	buf := defaultAllocator.Get(size + headerSize)
	buf.Reserve(headerSize)
	// tell sender we have no header, need make one.
	clear(buf.b[0:headerSize])
	return buf
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
			return &Buffer{b: make([]byte, 1<<uint32(i))}
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
	b.resetCursor()
	alloc.buffers[bits].Put(b)
	return nil
}

// msb return the pos of most significant bit
func msb(size int) uint16 {
	return uint16(bits.Len32(uint32(size)) - 1)
}
