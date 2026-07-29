package recv

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func TestRxGroupWindowCompletedGroupsDoNotAccumulate(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	for groupID := uint32(1); groupID <= 10_000; groupID++ {
		w.addData(groupID, 0, []byte{byte(groupID)})
		result, _ := w.addRepair(groupID, uint16(groupID), 1, []byte{byte(groupID)})
		if len(result.done) != 1 {
			t.Fatalf("group %d done groups = %d, want 1", groupID, len(result.done))
		}
		if size := w.groups.Size(); size != 0 {
			t.Fatalf("group %d active groups = %d, want 0", groupID, size)
		}
	}
}

func TestRxGroupWindowPrunesLowestGroupKey(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()
	w.maxGroups = 2

	w.addRepair(30, 1, 4, []byte("30"))
	w.addRepair(10, 2, 4, []byte("10"))
	result, _ := w.addRepair(20, 3, 4, []byte("20"))

	if size := w.groups.Size(); size != 2 {
		t.Fatalf("active groups = %d, want 2", size)
	}
	if _, ok := w.groups.Get(10); ok {
		t.Fatal("lowest group was not evicted")
	}
	for _, groupID := range []uint32{20, 30} {
		if _, ok := w.groups.Get(groupID); !ok {
			t.Fatalf("group %d was unexpectedly evicted", groupID)
		}
	}
	if len(result.done) != 1 || result.done[0].group.groupID != 10 || !result.done[0].expired {
		t.Fatalf("eviction result = %+v, want expired group 10", result.done)
	}
}

func TestRxGroupWindowPrunesLowestClosedKey(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()
	w.maxClosed = 2

	w.closeGroup(30)
	w.closeGroup(10)
	w.closeGroup(20)
	w.prune()

	if size := w.closed.Size(); size != 2 {
		t.Fatalf("closed groups = %d, want 2", size)
	}
	if _, ok := w.closed.Get(10); ok {
		t.Fatal("lowest closed group was not evicted")
	}
	for _, groupID := range []uint32{20, 30} {
		if _, ok := w.closed.Get(groupID); !ok {
			t.Fatalf("closed group %d was unexpectedly evicted", groupID)
		}
	}
}

func TestRxGroupWindowExpiresDataOnlyGroup(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	w.addData(100, 0, []byte("data"))
	result := w.expireGroup(100)
	if len(result.done) != 0 || len(result.recoverable) != 0 {
		t.Fatalf("DATA-only expiration result = %+v, want empty", result)
	}
	if group, _ := w.groups.Get(100); group != nil {
		t.Fatalf("DATA-only group survived expiration: %+v", group)
	}
	if _, closed := w.closed.Get(100); !closed {
		t.Fatal("expired DATA-only group was not closed")
	}
}

func TestRxGroupWindowClosedGroupDoesNotRetainLateData(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	w.addData(100, 0, []byte("first"))
	w.expireGroup(100)
	w.addData(100, 1, []byte("late"))
	if group, _ := w.groups.Get(100); group != nil {
		t.Fatalf("late DATA recreated closed group: %+v", group)
	}
}

func TestRxGroupWindowCompletionReleasesOwnedData(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	w.addData(100, 0, []byte("first"))
	w.addData(100, 1, []byte("second"))
	group, _ := w.groups.Get(100)
	if group == nil {
		t.Fatal("missing DATA group")
	}
	owned := append([]*packetbuf.Packet(nil), group.data[:2]...)

	result, _ := w.addRepair(100, 7, 2, []byte("repair"))
	if len(result.done) != 1 {
		t.Fatalf("completion result = %+v, want one done group", result)
	}
	if active, _ := w.groups.Get(100); active != nil {
		t.Fatalf("completed group survived: %+v", active)
	}
	for i, packet := range owned {
		if packet == nil || packet.Payload != nil {
			t.Fatalf("owned DATA %d was not released", i)
		}
		if group.data[i] != nil {
			t.Fatalf("group DATA %d still references released packet", i)
		}
	}
}

func TestRxGroupWindowRejectsRepairSpanBelowExistingDataIndex(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	w.addData(100, 3, []byte("data"))
	w.addRepair(100, 7, 2, []byte("repair"))
	group, _ := w.groups.Get(100)
	if group == nil || group.key.sourceSpan != 0 || len(group.repairs) != 0 {
		t.Fatalf("conflicting REPAIR changed group: %+v", group)
	}
}
