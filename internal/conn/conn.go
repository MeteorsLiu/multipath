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
	WriteBatch(b [][]byte) (n int64, err error)
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
