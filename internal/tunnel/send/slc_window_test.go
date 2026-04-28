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
