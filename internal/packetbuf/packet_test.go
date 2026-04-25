package packetbuf

import "testing"

func TestAcquireUsesSizeClasses(t *testing.T) {
	tests := []struct {
		size int
		cap  int
	}{
		{size: 0, cap: 1},
		{size: 1, cap: 1},
		{size: 1500, cap: 2048},
		{size: 64 * 1024, cap: 64 * 1024},
	}

	for _, tt := range tests {
		packet := Acquire(tt.size)
		if len(packet.Payload) != tt.size {
			t.Fatalf("size %d len = %d, want %d", tt.size, len(packet.Payload), tt.size)
		}
		if cap(packet.Payload) != tt.cap {
			t.Fatalf("size %d cap = %d, want %d", tt.size, cap(packet.Payload), tt.cap)
		}
		packet.Release()
	}
}

func TestPacketReleaseResetsPayload(t *testing.T) {
	packet := Acquire(1500)
	packet.SetLen(10)
	packet.Release()

	packet = Acquire(1500)
	if len(packet.Payload) != 1500 {
		t.Fatalf("Payload len after reuse = %d, want 1500", len(packet.Payload))
	}
	packet.Release()
}
