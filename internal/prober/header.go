package prober

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

// epoch + prober id
const ProbeHeaderSize = 1 + 16

func ProberIDFromBuffer(buf *mempool.Buffer) (byte, uuid.UUID, error) {
	epoch := buf.Peek(1)
	if epoch[0] > 1 {
		return 0, uuid.Nil, fmt.Errorf("probe packet has been replied")
	}
	id := buf.Peek(16)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return 0, uuid.Nil, fmt.Errorf("failed to parse probe packet header")
	}
	return epoch[0], proberId, nil
}

func incrEpoch(epoch byte, pkt *mempool.Buffer) {
	pkt.WriteByteAt(epoch+1, protocol.HeaderSize)
}
