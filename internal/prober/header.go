package prober

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const ProbeHeaderSize = 17

func ProberIDFromBuffer(buf *mempool.Buffer) (uuid.UUID, error) {
	epoch := buf.Peek(1)
	if epoch[0] > 0 {
		return uuid.Nil, fmt.Errorf("probe packet has been replied")
	}
	epoch[0]++
	buf.WriteAt(epoch, protocol.HeaderSize)

	id := buf.Peek(ProbeHeaderSize)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to parse probe packet header")
	}
	return proberId, nil
}
