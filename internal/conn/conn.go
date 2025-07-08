package conn

import (
	"errors"
	"io"
)

var ErrTooManySegments = errors.New("too many segments")

type BatchReader interface {
	ReadBatch(b [][]byte) (n int64, err error)
}

type BatchWriter interface {
	WriteBatch(b [][]byte) (n int64, err error)
}

type BatchConn interface {
	io.Closer
	BatchReader
	BatchWriter
}
