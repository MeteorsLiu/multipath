package batch

import (
	"io"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/sys/unix"
)

type batchWriter struct {
	fd syscall.RawConn

	iov *resizableIov
}

func NewWriter(fd syscall.RawConn) conn.BatchWriter {
	return &batchWriter{fd: fd, iov: newResizableIov()}
}

// WriteBatch acts like writeBuffers for net.Buffers
// However, tun device is underlying by *os.File, which dones't implement writeBuffers(), so we cannot use net.Buffers
func (f *batchWriter) WriteBatch(b [][]byte) (n int64, err error) {
	if len(b) == 0 {
		return
	}
	maxSize, err := f.iov.fill(b)
	if err != nil {
		return
	}
	// reset cursor to avoid buffers leaky
	defer f.iov.resetCursor()

	err2 := f.fd.Write(func(fd uintptr) (done bool) {
		var nw int

		for maxSize > 0 {
			offsetIoves := f.iov.iovec()
			// should not happen
			if len(offsetIoves) == 0 {
				panic("zero ioves")
			}
			nw, err = writev(int(fd), offsetIoves)

			if nw > 0 {
				n += int64(nw)
				maxSize -= int64(nw)

				if maxSize > 0 {
					f.iov.resize(n)
				}
			}

			switch err {
			case unix.EINTR:
				continue
			case unix.EAGAIN:
				return false
			}

			if err != nil {
				break
			}

			if nw == 0 {
				err = io.ErrUnexpectedEOF
				break
			}
		}

		return true
	})

	if err == nil && err2 != nil {
		err = err2
	}
	return
}
