package send

import "testing"

func TestTxSLCWindowEmitsContiguousGroup(t *testing.T) {
	window := newTxSLCWindow(4)
	for i := uint32(100); i < 103; i++ {
		if _, ok := window.add(i, []byte{byte(i)}); ok {
			t.Fatalf("unexpected repair group before four packets")
		}
	}

	group, ok := window.add(103, []byte{103})
	if !ok {
		t.Fatal("expected repair group")
	}
	if group.basePacketID != 100 {
		t.Fatalf("basePacketID = %d, want 100", group.basePacketID)
	}
	for i := 0; i < 4; i++ {
		if got := group.packets[i].Payload[0]; got != byte(100+i) {
			t.Fatalf("packet[%d] = %d, want %d", i, got, 100+i)
		}
	}
	for _, pkt := range group.packets {
		pkt.Release()
	}
}

func TestTxSLCWindowSkipsNonContiguousStart(t *testing.T) {
	window := newTxSLCWindow(4)
	for _, packetID := range []uint32{100, 102, 103, 104} {
		if _, ok := window.add(packetID, []byte{byte(packetID)}); ok {
			t.Fatalf("unexpected repair group for non-contiguous start")
		}
	}

	group, ok := window.add(105, []byte{105})
	if !ok {
		t.Fatal("expected repair group after skipping missing packet 101")
	}
	if group.basePacketID != 102 {
		t.Fatalf("basePacketID = %d, want 102", group.basePacketID)
	}
	for _, pkt := range group.packets {
		pkt.Release()
	}
}

func TestTxSLCWindowCopiesPackets(t *testing.T) {
	window := newTxSLCWindow(4)
	packet := []byte{1}
	window.add(1, packet)
	packet[0] = 9
	window.add(2, []byte{2})
	window.add(3, []byte{3})
	group, ok := window.add(4, []byte{4})
	if !ok {
		t.Fatal("expected repair group")
	}
	if got := group.packets[0].Payload[0]; got != 1 {
		t.Fatalf("packet copy = %d, want 1", got)
	}
	for _, pkt := range group.packets {
		pkt.Release()
	}
}

func TestTxSLCWindowFlushDrainsContiguousPrefix(t *testing.T) {
	for _, count := range []int{1, 2, 3} {
		window := newTxSLCWindow(4)
		for i := 0; i < count; i++ {
			if _, ok := window.add(uint32(100+i), []byte{byte(i)}); ok {
				t.Fatalf("count %d: unexpected full group", count)
			}
		}

		group, ok := window.flush()
		if !ok {
			t.Fatalf("count %d: flush ok = false", count)
		}
		if group.basePacketID != 100 || int(group.sourceSpan) != count || len(group.packets) != count {
			t.Fatalf("count %d: group = base %d sourceSpan %d packets %d", count, group.basePacketID, group.sourceSpan, len(group.packets))
		}
		if len(window.pending) != 0 {
			t.Fatalf("count %d: pending = %d, want 0", count, len(window.pending))
		}
		for _, pkt := range group.packets {
			pkt.Release()
		}
	}
}

func TestTxSLCWindowFlushLeavesTailAfterGap(t *testing.T) {
	window := newTxSLCWindow(4)
	for _, packetID := range []uint32{100, 101, 103} {
		if _, ok := window.add(packetID, []byte{byte(packetID)}); ok {
			t.Fatal("unexpected full group")
		}
	}

	group, ok := window.flush()
	if !ok {
		t.Fatal("flush ok = false")
	}
	if int(group.sourceSpan) != 2 {
		t.Fatalf("sourceSpan = %d, want 2", group.sourceSpan)
	}
	for _, pkt := range group.packets {
		pkt.Release()
	}
	if len(window.pending) != 1 || window.pending[0].packetID != 103 {
		t.Fatalf("pending tail = %+v, want packet 103", window.pending)
	}
	window.releaseAll()
}
