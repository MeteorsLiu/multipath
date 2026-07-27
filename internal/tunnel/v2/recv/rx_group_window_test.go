package recv

import "testing"

func TestRxGroupWindowCompletedGroupsDoNotAccumulate(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	for packetID := uint32(1); packetID <= 10_000; packetID++ {
		w.addData(packetID, []byte{byte(packetID)})
		result := w.addRepair(packetID, uint16(packetID), 1, []byte{byte(packetID)})
		if len(result.done) != 1 {
			t.Fatalf("packet %d done groups = %d, want 1", packetID, len(result.done))
		}
		if size := w.groups.Size(); size != 0 {
			t.Fatalf("packet %d active groups = %d, want 0", packetID, size)
		}
	}
}

func TestRxGroupWindowPrunesLowestGroupKey(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()
	w.maxGroups = 2

	w.addRepair(30, 1, 4, []byte("30"))
	w.addRepair(10, 2, 4, []byte("10"))
	result := w.addRepair(20, 3, 4, []byte("20"))

	if size := w.groups.Size(); size != 2 {
		t.Fatalf("active groups = %d, want 2", size)
	}
	if _, ok := w.groups.Get(rxGroupKey{basePacketID: 10, sourceSpan: 4}); ok {
		t.Fatal("lowest group was not evicted")
	}
	for _, packetID := range []uint32{20, 30} {
		if _, ok := w.groups.Get(rxGroupKey{basePacketID: packetID, sourceSpan: 4}); !ok {
			t.Fatalf("group %d was unexpectedly evicted", packetID)
		}
	}
	if len(result.done) != 1 || result.done[0].group.basePacketID != 10 || !result.done[0].expired {
		t.Fatalf("eviction result = %+v, want expired group 10", result.done)
	}
}
