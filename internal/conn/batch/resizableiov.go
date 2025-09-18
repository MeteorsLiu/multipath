package batch

import (
	"sort"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/sys/unix"
)

type iovesMetadata struct {
	buf      []byte
	metadata int64
}

type resizableIov struct {
	pos      int
	sum      int64
	ioves    []unix.Iovec
	metadata []*iovesMetadata
}

func calculateConsumed(metadata []*iovesMetadata, offset int, submitted int64) (
	offsetPtr *byte,
	availableBytes int,
) {
	currentBuf := metadata[offset].buf

	fullBytes := len(currentBuf)
	availableBytes = int(metadata[offset].metadata - submitted)

	// bound edge: when we have no availableBytes, no need to resize because pos cursor has move to the end
	// this is only for safety
	if availableBytes > 0 {
		consumedBytes := fullBytes - availableBytes
		offsetPtr = &currentBuf[consumedBytes]
	}
	return
}

func newResizableIov() *resizableIov {
	return &resizableIov{ioves: make([]unix.Iovec, 0, 1024), metadata: make([]*iovesMetadata, 0, 1024)}
}

func (f *resizableIov) append(b []byte) {
	f.sum += int64(len(b))
	f.metadata = append(f.metadata, &iovesMetadata{
		buf:      b,
		metadata: f.sum,
	})
	f.ioves = f.ioves[:len(f.metadata)]
	f.ioves[len(f.metadata)-1].Base = &b[0]
	f.ioves[len(f.metadata)-1].SetLen(len(b))
}

func (f *resizableIov) fill(b [][]byte) (int64, error) {
	if len(b) > 1024 {
		return 0, conn.ErrTooManySegments
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

func (f *resizableIov) find(size int64, next bool) int {
	metadata := f.metadata[f.pos:]

	startIdx := 0

	if metadata[0].metadata <= size {
		i := sort.Search(len(metadata), func(i int) bool {
			// consider end bound edge (Test case 5th(Edge)), so we need to search the previous one
			return metadata[i].metadata >= size
		})
		// we got a previous result, but we need a greater one, so move to next one
		// be careful about the slice bound.
		if next && metadata[i].metadata <= size {
			i = min(i+1, len(metadata))
		}

		startIdx = i
	}

	return startIdx
}

func (f *resizableIov) resize(submitted int64) {
	metadata := f.metadata[f.pos:]
	startIdx := f.find(submitted, true)

	pos := f.pos + startIdx

	if startIdx < len(metadata) {
		consumedBufPtr, availableBytes := calculateConsumed(metadata, startIdx, submitted)

		f.ioves[pos].Base = consumedBufPtr
		f.ioves[pos].SetLen(availableBytes)
	}

	f.pos = pos
}

func (f *resizableIov) iovec() []unix.Iovec {
	return f.ioves[f.pos:]
}

func (f *resizableIov) resetCursor() {
	f.pos = 0
	f.sum = 0
	f.ioves = f.ioves[:0]
	f.metadata = f.metadata[:0]
}
