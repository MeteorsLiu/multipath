package conn

import (
	"errors"
	"io"

	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type ManagerEvent int

const (
	ConnNop ManagerEvent = iota
	ConnAppend
	ConnRemove
)

var ErrTooManySegments = errors.New("too many segments")

type BatchReader interface {
	ReadBatch(b [][]byte) (nums int, n int64, err error)
}

type BatchWriter interface {
	io.Writer
	Submit() (n int64, err error)
}

type BatchConn interface {
	io.Closer
	BatchReader
	BatchWriter
}

type MuxConn interface {
}

type ConnWriter interface {
	mempool.Writer
	String() string
}

type ByteWriterAt interface {
	WriteByteAt(c byte, off int) error
}
