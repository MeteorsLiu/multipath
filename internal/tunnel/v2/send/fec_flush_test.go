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
			packet: packet,
		})
	}
	window.nextIndex = maxFECSourceSpan

	if group, ok := window.flush(); ok {
		t.Fatalf("flush produced source_span=%d for a full pending group", group.sourceSpan)
	}
	if len(window.pending) != maxFECSourceSpan {
		t.Fatalf("pending = %d, want full group retained", len(window.pending))
	}
}

func TestTxSLCWindowAllocatesLaneLocalGroupAndSourceIndex(t *testing.T) {
	window := newTxSLCWindow(maxFECSourceSpan)
	want := []struct {
		groupID uint32
		index   uint8
	}{{0, 0}, {0, 1}, {0, 2}, {0, 3}, {1, 0}}

	for i, expected := range want {
		groupID, index, _, _ := window.add(nil, false)
		if groupID != expected.groupID || index != expected.index {
			t.Fatalf("DATA %d id = group:%d index:%d, want group:%d index:%d", i, groupID, index, expected.groupID, expected.index)
		}
	}
}

func TestTxSLCWindowGroupIDsAreLaneLocal(t *testing.T) {
	lane1 := newLaneRuntime(1, 100)
	lane2 := newLaneRuntime(2, 100)

	group1, index1, _, _, _ := lane1.commitPacket(nil, false)
	group2, index2, _, _, _ := lane2.commitPacket(nil, false)
	if group1 != 0 || index1 != 0 || group2 != 0 || index2 != 0 {
		t.Fatalf("first ids = lane1:%d/%d lane2:%d/%d, want both 0/0", group1, index1, group2, index2)
	}
}

func TestTxSLCWindowPartialFlushStartsNextGroup(t *testing.T) {
	window := newTxSLCWindow(maxFECSourceSpan)
	defer window.releaseAll()

	window.add([]byte("a"), true)
	window.add([]byte("b"), true)
	group, ok := window.flush()
	if !ok {
		t.Fatal("partial group was not flushed")
	}
	if group.groupID != 0 || group.sourceSpan != 2 {
		t.Fatalf("flushed group = id:%d span:%d, want id:0 span:2", group.groupID, group.sourceSpan)
	}
	for _, packet := range group.packets {
		packet.Release()
	}

	groupID, index, _, _ := window.add(nil, false)
	if groupID != 1 || index != 0 {
		t.Fatalf("DATA after partial flush = group:%d index:%d, want group:1 index:0", groupID, index)
	}
}

func TestTxSLCWindowFECEnableStartsFreshGroup(t *testing.T) {
	window := newTxSLCWindow(maxFECSourceSpan)
	defer window.releaseAll()

	window.add(nil, false)
	window.add(nil, false)
	groupID, index, _, _ := window.add([]byte("protected"), true)
	if groupID != 1 || index != 0 {
		t.Fatalf("first protected DATA = group:%d index:%d, want group:1 index:0", groupID, index)
	}
	if len(window.pending) != 1 {
		t.Fatalf("pending protected packets = %d, want 1", len(window.pending))
	}
}

func TestTxSLCWindowGroupIDWrapsWithoutStoppingData(t *testing.T) {
	window := newTxSLCWindow(maxFECSourceSpan)
	window.nextGroupID = maxTxGroupID

	for index := uint8(0); index < maxFECSourceSpan; index++ {
		groupID, gotIndex, _, _ := window.add(nil, false)
		if groupID != maxTxGroupID || gotIndex != index {
			t.Fatalf("last group DATA = group:%d index:%d, want group:%d index:%d", groupID, gotIndex, maxTxGroupID, index)
		}
	}
	groupID, index, _, _ := window.add(nil, false)
	if groupID != 0 || index != 0 {
		t.Fatalf("DATA after wrap = group:%d index:%d, want group:0 index:0", groupID, index)
	}
}
