package prober

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const ProbeHeaderSize = 17

func ProberIDFromBuffer(buf *mempool.Buffer) ([]byte, uuid.UUID, error) {
	epoch := buf.Peek(1)
	if epoch[0] > 0 {
		return nil, uuid.Nil, fmt.Errorf("probe packet has been replied")
	}

	id := buf.Peek(16)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("failed to parse probe packet header")
	}
	return epoch, proberId, nil
}

func IncrEpoch(epoch []byte, pkt *mempool.Buffer) {
	epoch[0]++
	pkt.WriteAt(epoch, protocol.HeaderSize)
}
