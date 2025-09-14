package batch

import (
	"fmt"
	"io"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"golang.org/x/sys/unix"
)

type batchWriter struct {
	fd syscall.RawConn

	iov *resizableIov
}

func NewWriter(fd syscall.RawConn) conn.BatchWriter {
	return &batchWriter{fd: fd, iov: newResizableIov()}
}

func (f *batchWriter) Write(b *mempool.Buffer) error {
	f.iov.append(b.FullBytes())
	return nil
}

// WriteBatch acts like writeBuffers for net.Buffers
// However, tun device is underlying by *os.File, which dones't implement writeBuffers(), so we cannot use net.Buffers
func (f *batchWriter) Submit() (n int64, err error) {
	if len(f.iov.ioves) == 0 {
		err = fmt.Errorf("no buffer ")
		return
	}
	maxSize := f.iov.sum
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
