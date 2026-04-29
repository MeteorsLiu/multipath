package recv

import "testing"

func TestRxSLCWindowRepairBecomesRecoverable(t *testing.T) {
	window := newRxSLCWindow(4)
	window.addData(100, []byte("a"))
	window.addData(102, []byte("c"))
	window.addData(103, []byte("d"))

	recoverable, ok := window.addRepair(100, 7, 4, []byte("repair"))
	if !ok {
		t.Fatal("expected recoverable group with one missing packet")
	}
	if recoverable.basePacketID != 100 {
		t.Fatalf("basePacketID = %d, want 100", recoverable.basePacketID)
	}
	if recoverable.key != 7 {
		t.Fatalf("key = %d, want 7", recoverable.key)
	}
	if recoverable.missingIndex != 1 {
		t.Fatalf("missingIndex = %d, want 1", recoverable.missingIndex)
	}
	shards, ok := window.buildShardsLocked(recoverable, nil)
	if !ok {
		t.Fatal("buildShardsLocked: ok = false")
	}
	if string(shards[0]) != "a" || string(shards[2]) != "c" || string(shards[3]) != "d" || string(shards[4]) != "repair" {
		t.Fatalf("unexpected shards: %#v", shards)
	}
}

func TestRxSLCWindowDataCompletesStoredRepair(t *testing.T) {
	window := newRxSLCWindow(4)
	window.addData(100, []byte("a"))
	window.addData(102, []byte("c"))
	if _, ok := window.addRepair(100, 7, 4, []byte("repair")); ok {
		t.Fatal("repair should not recover with two missing packets")
	}

	recoverable, ok := window.addData(103, []byte("d"))
	if !ok {
		t.Fatal("expected data to complete stored repair")
	}
	if recoverable.missingIndex != 1 {
		t.Fatalf("missingIndex = %d, want 1", recoverable.missingIndex)
	}
}

func TestRxSLCWindowDropsRepairWhenAllDataKnown(t *testing.T) {
	window := newRxSLCWindow(4)
	window.addData(100, []byte("a"))
	window.addData(101, []byte("b"))
	window.addData(102, []byte("c"))
	window.addData(103, []byte("d"))

	if _, ok := window.addRepair(100, 7, 4, []byte("repair")); ok {
		t.Fatal("repair should not recover when all data is known")
	}
	if len(window.repairs) != 0 {
		t.Fatalf("stored repairs = %d, want 0", len(window.repairs))
	}
}

func TestRxSLCWindowDropsStoredRepairWhenDataCompletesWindow(t *testing.T) {
	window := newRxSLCWindow(4)
	window.addData(100, []byte("a"))
	window.addData(101, []byte("b"))
	if _, ok := window.addRepair(100, 7, 4, []byte("repair")); ok {
		t.Fatal("repair should not recover with two missing packets")
	}
	if len(window.repairs) != 1 {
		t.Fatalf("stored repairs = %d, want 1", len(window.repairs))
	}

	if _, ok := window.addData(102, []byte("c")); !ok {
		t.Fatal("expected repair to become recoverable with one missing packet")
	}
	if _, ok := window.addData(103, []byte("d")); ok {
		t.Fatal("data should not trigger recovery when it completes all symbols")
	}
	if len(window.repairs) != 0 {
		t.Fatalf("stored repairs = %d, want 0", len(window.repairs))
	}
}

func TestRxSLCWindowCopiesInputs(t *testing.T) {
	window := newRxSLCWindow(4)
	packet := []byte("a")
	repair := []byte("repair")
	window.addData(100, packet)
	window.addData(102, []byte("c"))
	window.addData(103, []byte("d"))
	packet[0] = 'x'

	recoverable, ok := window.addRepair(100, 7, 4, repair)
	if !ok {
		t.Fatal("expected recoverable group")
	}
	repair[0] = 'x'
	shards, ok := window.buildShardsLocked(recoverable, nil)
	if !ok {
		t.Fatal("buildShardsLocked: ok = false")
	}
	if string(shards[0]) != "a" {
		t.Fatalf("data shard = %q, want a", shards[0])
	}
	if string(shards[4]) != "repair" {
		t.Fatalf("repair shard = %q, want repair", shards[4])
	}
}

func TestRxSLCWindowPrunesDataAndEmittedMarks(t *testing.T) {
	window := newRxSLCWindow(4)
	window.maxData = 3

	for packetID := uint32(100); packetID < 104; packetID++ {
		window.addData(packetID, []byte{byte(packetID)})
		window.markEmitted(packetID)
	}

	if len(window.data) != 3 {
		t.Fatalf("data entries = %d, want 3", len(window.data))
	}
	if window.data[100] != nil {
		t.Fatal("oldest data packet was not pruned")
	}
	if window.emitted[100] {
		t.Fatal("oldest emitted marker was not pruned")
	}
	if window.data[101] == nil || window.data[102] == nil || window.data[103] == nil {
		t.Fatalf("unexpected retained data: %#v", window.data)
	}
}

func TestRxSLCWindowPrunesRepairs(t *testing.T) {
	window := newRxSLCWindow(4)
	window.maxRepairs = 2

	window.addRepair(100, 1, 4, []byte("a"))
	window.addRepair(104, 2, 4, []byte("b"))
	window.addRepair(108, 3, 4, []byte("c"))

	if len(window.repairs) != 2 {
		t.Fatalf("repair entries = %d, want 2", len(window.repairs))
	}
	if _, ok := window.repairs[100]; ok {
		t.Fatal("oldest repair was not pruned")
	}
	if _, ok := window.repairs[104]; !ok {
		t.Fatal("repair 104 was pruned unexpectedly")
	}
	if _, ok := window.repairs[108]; !ok {
		t.Fatal("repair 108 was pruned unexpectedly")
	}
}
