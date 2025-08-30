package prober

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const ProbeHeaderSize = 16

func ProberIDFromBuffer(buf *mempool.Buffer) (uuid.UUID, error) {
	id := buf.Peek(ProbeHeaderSize)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to parse probe packet header")
	}
	return proberId, nil
}
