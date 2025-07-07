package batch

import (
	"io"
	"sort"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/sys/unix"
)

type iovesMetadata struct {
	buf      []byte
	metadata int64
}

type batchWriter struct {
	fd syscall.RawConn

	pos      int
	ioves    []unix.Iovec
	metadata []*iovesMetadata
}

func NewWriter(fd syscall.RawConn) conn.BatchWriter {
	return &batchWriter{fd: fd, ioves: make([]unix.Iovec, 1024), metadata: make([]*iovesMetadata, 1024)}
}

func (f *batchWriter) fillIov(b [][]byte) (int64, error) {
	if len(b) > 1024 {
		return 0, ErrTooManySegments
	}
	if len(f.ioves) > 0 || len(f.metadata) > 0 {
		panic("the cursor of metadatas or ioves dones't reset.")
	}

	var sum int64

	for _, buf := range b {
		var vec unix.Iovec
		vec.Base = &buf[0]
		vec.SetLen(len(buf))

		sum += int64(len(buf))

		f.ioves = append(f.ioves, vec)
		f.metadata = append(f.metadata, &iovesMetadata{
			metadata: sum,
			buf:      buf,
		})
	}

	return sum, nil
}

func calculateConsumed(metadata []*iovesMetadata, offset int, submitted int64) (
	offsetPtr *byte,
	availableBytes int,
) {
	currentBuf := metadata[offset].buf

	fullBytes := len(currentBuf)
	availableBytes = int(metadata[offset].metadata - submitted)

	consumedBytes := fullBytes - availableBytes
	offsetPtr = &currentBuf[consumedBytes]
	return
}

func (f *batchWriter) resizeIov(submitted int64) {
	metadata := f.metadata[f.pos:]

	pos := f.pos
	startIdx := 0

	// if the first metadata is greater than submitted, just consume the first buffer
	// bound edge: when first buffer size is equal to submitted, we should move to next one
	if metadata[0].metadata <= submitted {
		i := sort.Search(len(metadata), func(i int) bool {
			return metadata[i].metadata > submitted
		})
		startIdx = i
		pos += i
	}
	consumedBufPtr, availableBytes := calculateConsumed(metadata, startIdx, submitted)

	f.ioves[pos].Base = consumedBufPtr
	f.ioves[pos].SetLen(availableBytes)

	f.pos = pos
}

func (f *batchWriter) resetCursor() {
	f.pos = 0
	f.ioves = f.ioves[:0]
	f.metadata = f.metadata[:0]
}

// WriteBatch acts like writeBuffers for net.Buffers
// However, tun device is underlying by *os.File, which dones't implement writeBuffers(), so we cannot use net.Buffers
func (f *batchWriter) WriteBatch(b [][]byte) (n int64, err error) {
	maxSize, err := f.fillIov(b)
	if err != nil {
		return
	}
	// reset cursor to avoid buffers leaky
	defer f.resetCursor()

	err2 := f.fd.Write(func(fd uintptr) (done bool) {
		var nw int

		for maxSize > 0 {
			nw, err = writev(int(fd), f.ioves[f.pos:])

			if nw > 0 {
				n += int64(nw)
				maxSize -= int64(nw)

				if maxSize > 0 {
					f.resizeIov(n)
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
