package recv

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

// BenchmarkRecvWriteToDATA measures the cost of feeding a wire-format DATA
// frame through Recv.WriteTo on the receive hot path. The benchmark patches
// the encoded packet's PacketID per iteration so each call exercises a fresh
// window slot rather than the duplicate-suppression fast exit. The decoded
// IP packet channel is drained by a separate goroutine.
func BenchmarkRecvWriteToDATA(b *testing.B) {
	benchmarkRecvWriteToDATA(b, 1436)
}

func benchmarkRecvWriteToDATA(b *testing.B, payloadLen int) {
	b.Helper()
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		b.Fatal("Create session")
	}
	out := New(Config{SessionManager: &manager})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go drainRecvPackets(ctx, out, done)
	defer func() {
		cancel()
		<-done
	}()

	dataPayload := make([]byte, payloadLen)
	for i := range dataPayload {
		dataPayload[i] = byte(i)
	}
	encoded, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    1,
		Body:      protocol.DataBody{PacketID: 0, Packet: dataPayload},
	}, nil)
	if err != nil {
		b.Fatalf("Encode: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Patch the PacketID so each iteration is a fresh window slot.
		binary.BigEndian.PutUint32(encoded[10:14], uint32(i))
		packet := packetbuf.Acquire(len(encoded))
		copy(packet.Payload, encoded)
		packet.SetLen(len(encoded))
		if err := out.WriteTo(ctx, transport.LegRef{}, packet); err != nil {
			b.Fatalf("WriteTo: %v", err)
		}
	}
}

func drainRecvPackets(ctx context.Context, out *Recv, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case packet := <-out.Packets():
			packet.Release()
		}
	}
}
