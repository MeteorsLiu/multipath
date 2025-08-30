package prober

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const ProbeHeaderSize = 17

func ProberIDFromBuffer(buf *mempool.Buffer) (byte, uuid.UUID, error) {
	epoch := buf.Peek(1)
	if epoch[0] > 0 {
		return 0, uuid.Nil, fmt.Errorf("probe packet has been replied")
	}
	id := buf.Peek(16)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return 0, uuid.Nil, fmt.Errorf("failed to parse probe packet header")
	}
	return epoch[0], proberId, nil
}

func IncrEpoch(epoch byte, pkt *mempool.Buffer) {
	var buf [1]byte
	buf[0] = epoch
	pkt.WriteAt(buf[:], protocol.HeaderSize)
}
