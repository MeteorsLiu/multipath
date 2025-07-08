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

func (f *resizableIov) resize(submitted int64) {
	metadata := f.metadata[f.pos:]

	pos := f.pos
	startIdx := 0

	// if the first metadata is greater than submitted, just consume the first buffer
	// bound edge: when first buffer size is equal to submitted, we should move to next one
	if metadata[0].metadata <= submitted {
		i := sort.Search(len(metadata), func(i int) bool {
			// consider end bound edge (Test case 5th(Edge)), so we need to search the previous one
			return metadata[i].metadata >= submitted
		})
		// we got a previous result, but we need a greater one, so move to next one
		// be careful about the slice bound.
		if metadata[i].metadata <= submitted {
			i = min(i+1, len(metadata))
		}

		startIdx = i
		pos += i
	}

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
	f.ioves = f.ioves[:0]
	f.metadata = f.metadata[:0]
}
