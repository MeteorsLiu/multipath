package recv

import (
	"context"
	"testing"

	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
)

func TestEmitDedupeMarksFirstSeenOnce(t *testing.T) {
	d := newEmitDedupe(0)
	if !d.mark(7) {
		t.Fatal("first mark(7) = false, want true")
	}
	if d.mark(7) {
		t.Fatal("second mark(7) = true, want false (duplicate)")
	}
	if !d.mark(8) {
		t.Fatal("mark(8) = false, want true")
	}
}

func TestEmitDedupeAllowsReorderedEarlierID(t *testing.T) {
	d := newEmitDedupe(0)
	if !d.mark(105) {
		t.Fatal("mark(105) = false, want true")
	}
	// A reordered-earlier id below the first one seen must still be accepted.
	if !d.mark(103) {
		t.Fatal("mark(103) after 105 = false, want true (reordered earlier id)")
	}
	if d.mark(103) {
		t.Fatal("duplicate mark(103) = true, want false")
	}
}

func TestEmitDedupeEvictsIDsOlderThanWindow(t *testing.T) {
	d := newEmitDedupe(8) // tiny window
	if !d.mark(0) {
		t.Fatal("mark(0) = false, want true")
	}
	// Slide far past the window so id 0 is evicted.
	if !d.mark(64) {
		t.Fatal("mark(64) = false, want true")
	}
	// id 0 now sorts before the window base: treated as already emitted.
	if d.mark(0) {
		t.Fatal("mark(0) after eviction = true, want false")
	}
}

func TestEmitDedupeHandlesUint32Wraparound(t *testing.T) {
	d := newEmitDedupe(0)
	high := ^uint32(0) // 0xFFFFFFFF
	if !d.mark(high - 1) {
		t.Fatal("mark(max-1) = false, want true")
	}
	if !d.mark(high) {
		t.Fatal("mark(max) = false, want true")
	}
	// Wrap forward past zero.
	if !d.mark(0) {
		t.Fatal("mark(0) after wrap = false, want true")
	}
	if !d.mark(1) {
		t.Fatal("mark(1) after wrap = false, want true")
	}
	// The pre-wrap id is still inside the trailing window: duplicate.
	if d.mark(high) {
		t.Fatal("duplicate mark(max) after wrap = true, want false")
	}
}

// TestDuplicateDataDoesNotEnterRxWindow verifies a duplicate DATA frame is
// dropped by the session dedupe before it reaches the FEC receive window.
func TestDuplicateDataDoesNotEnterRxWindow(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	dataFrame := protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 5, Packet: []byte("packet")},
	}
	if err := out.Write(context.Background(), encodedTestFrame(t, dataFrame)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	first := readRecvPacket(t, out)
	first.Release()

	// Duplicate of the same packet id.
	if err := out.Write(context.Background(), encodedTestFrame(t, dataFrame)); err != nil {
		t.Fatalf("Write duplicate DATA: %v", err)
	}
	assertNoRecvPacket(t, out)

	state := out.recvState(99)
	state.mu.Lock()
	n := len(state.rxWindow.data)
	state.mu.Unlock()
	if n != 1 {
		t.Fatalf("rxWindow data entries = %d, want 1 (duplicate must not enter window)", n)
	}
}

// TestRecoveredPacketUsesSessionDeduping verifies an FEC-recovered packet is
// emitted exactly once through the session dedupe and that the late-arriving
// original DATA for the recovered id is then dropped.
func TestRecoveredPacketUsesSessionDeduping(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	recovered := ipv4Packet(20)
	codec, err := fecpkg.NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{[]byte("a"), recovered, []byte("c"), []byte("d"), nil}
	if err := codec.Encode(shards, 7); err != nil {
		t.Fatalf("Encode repair: %v", err)
	}

	// DATA 100, 102, 103 arrive; 101 is missing.
	for _, item := range []struct {
		packetID uint32
		packet   []byte
	}{
		{100, []byte("a")},
		{102, []byte("c")},
		{103, []byte("d")},
	} {
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 99,
			LaneID:    3,
			Body:      protocol.DataBody{PacketID: item.packetID, Packet: item.packet},
		}
		if err := out.Write(context.Background(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA %d: %v", item.packetID, err)
		}
	}

	repair := protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.RepairBody{BasePacketID: 100, Key: 7, SourceSpan: 4, Symbol: shards[4]},
	}
	if err := out.Write(context.Background(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	got := collectRecvPackets(out)
	if len(got) != 4 {
		t.Fatalf("emitted packets = %d, want 4 (3 data + 1 recovered)", len(got))
	}
	if string(got[3]) != string(recovered) {
		t.Fatalf("fourth emit = %q, want recovered packet", got[3])
	}

	// Late original for the recovered id must be deduped away.
	lateOriginal := protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 101, Packet: recovered},
	}
	if err := out.Write(context.Background(), encodedTestFrame(t, lateOriginal)); err != nil {
		t.Fatalf("Write late original DATA: %v", err)
	}
	if extra := collectRecvPackets(out); len(extra) != 0 {
		t.Fatalf("late original emitted %d packets, want 0", len(extra))
	}
}

func collectRecvPackets(out *Recv) [][]byte {
	var got [][]byte
	for {
		select {
		case packet := <-out.Packets():
			got = append(got, append([]byte(nil), packet.Payload...))
			packet.Release()
		default:
			return got
		}
	}
}

func ipv4Packet(totalLen int) []byte {
	packet := make([]byte, totalLen)
	packet[0] = 0x45
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	for i := 20; i < totalLen; i++ {
		packet[i] = byte(i)
	}
	return packet
}
