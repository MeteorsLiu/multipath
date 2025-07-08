package batch

import (
	"io"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/sys/unix"
)

type batchReader struct {
	fd syscall.RawConn

	ioves []unix.Iovec
}

func NewReader(fd syscall.RawConn) conn.BatchReader {
	return &batchReader{fd: fd, ioves: make([]unix.Iovec, 0, 1024)}
}

func (f *batchReader) fillIov(b [][]byte) error {
	if len(b) > 1024 {
		return conn.ErrTooManySegments
	}
	if len(f.ioves) > 0 {
		panic("the cursor of ioves dones't reset.")
	}

	for _, buf := range b {
		var vec unix.Iovec
		vec.Base = &buf[0]
		vec.SetLen(len(buf))

		f.ioves = append(f.ioves, vec)
	}

	return nil
}

func (f *batchReader) resetCursor() {
	f.ioves = f.ioves[:0]
}

func (f *batchReader) ReadBatch(b [][]byte) (n int64, err error) {
	if len(b) == 0 {
		return
	}
	if err = f.fillIov(b); err != nil {
		return
	}
	defer f.resetCursor()

	err2 := f.fd.Read(func(fd uintptr) (done bool) {
		var nr int
		for {
			nr, err = readv(int(fd), f.ioves)

			if nr > 0 {
				n += int64(nr)
			}

			switch err {
			case unix.EINTR:
				// The read operation was terminated due to the receipt of a
				// signal, and no data was transferred.
				continue
			case unix.EAGAIN:
				return false
			}

			// EOF case
			if nr == 0 && err == nil {
				err = io.EOF
			}

			return true
		}
	})

	if err == nil && err2 != nil {
		err = err2
	}
	return
}
