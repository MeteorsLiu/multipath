package batch

import (
	"io"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/sys/unix"
)

type batchReader struct {
	fd syscall.RawConn

	ioves *resizableIov
}

func NewReader(fd syscall.RawConn) conn.BatchReader {
	return &batchReader{fd: fd, ioves: newResizableIov()}
}

func (f *batchReader) ReadBatch(b [][]byte) (nums int, n int64, err error) {
	if len(b) == 0 {
		return
	}
	_, err = f.ioves.fill(b)
	if err != nil {
		return
	}
	defer f.ioves.resetCursor()

	err2 := f.fd.Read(func(fd uintptr) (done bool) {
		var nr int
		for {
			nr, err = readv(int(fd), f.ioves.iovec())

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
