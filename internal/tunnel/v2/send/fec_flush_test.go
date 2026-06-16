package send

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func TestDefaultFECFlushDelayIsFiveMilliseconds(t *testing.T) {
	s := New()
	if s.fecFlushMin != 5*time.Millisecond {
		t.Fatalf("fecFlushMin = %s, want 5ms", s.fecFlushMin)
	}
}

func TestFECFlushSkipsFullPendingGroup(t *testing.T) {
	window := newTxSLCWindow(maxFECSourceSpan)
	defer window.releaseAll()

	for i := 0; i < maxFECSourceSpan; i++ {
		packet := packetbuf.Acquire(16)
		copy(packet.Payload, []byte("fec-flush-guard"))
		packet.SetLen(15)
		window.pending = append(window.pending, txSymbol{
			packetID: uint32(i),
			packet:   packet,
		})
	}

	if group, ok := window.flush(); ok {
		t.Fatalf("flush produced source_span=%d for a full pending group", group.sourceSpan)
	}
	if len(window.pending) != maxFECSourceSpan {
		t.Fatalf("pending = %d, want full group retained", len(window.pending))
	}
}
