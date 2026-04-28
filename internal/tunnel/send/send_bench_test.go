package send

import (
	"context"
	"net"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func BenchmarkRunnableLanesCached(b *testing.B) {
	in := newBenchmarkSendWithReadyLanes(8)
	in.runnableLanes(99)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if lanes := in.runnableLanes(99); len(lanes) != 8 {
			b.Fatalf("runnable lanes = %d, want 8", len(lanes))
		}
	}
}

func BenchmarkRunnableLanesRebuild(b *testing.B) {
	in := newBenchmarkSendWithReadyLanes(8)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		in.markRunnableLanesDirty(99)
		if lanes := in.runnableLanes(99); len(lanes) != 8 {
			b.Fatalf("runnable lanes = %d, want 8", len(lanes))
		}
	}
}

func newBenchmarkSendWithReadyLanes(count int) *Send {
	in := New()
	for i := 1; i <= count; i++ {
		laneID := uint8(i)
		lane := newLaneRuntime(laneID, 1)
		lane.observeLeg(transport.LegRef{
			Kind:       transport.KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10000 + i},
		})
		in.lanes[laneKey{sessionID: 99, laneID: laneID}] = lane
	}
	return in
}

// BenchmarkSendWriteUDP measures the cost of pushing a TUN packet through
// Send.Write onto a single ready UDP lane. The outbound transport channel is
// drained continuously so the benchmark observes the steady-state cost of the
// Send hot path rather than channel back-pressure.
func BenchmarkSendWriteUDP(b *testing.B) {
	benchmarkSendWriteUDP(b, 1, 1436)
}

// BenchmarkSendWriteUDP4Lanes exercises the schedule strategy with multiple
// runnable lanes.
func BenchmarkSendWriteUDP4Lanes(b *testing.B) {
	benchmarkSendWriteUDP(b, 4, 1436)
}

// BenchmarkSendWriteUDPFEC exercises the production-default FEC-on path so
// the tx FEC window's per-packet cost is visible to benchmarks.
func BenchmarkSendWriteUDPFEC(b *testing.B) {
	in := newBenchmarkSendWithReadyLanes(1)
	in.enableFEC()
	benchmarkSendWriteUDPInstance(b, in, 1436)
}

func benchmarkSendWriteUDP(b *testing.B, lanes, payloadLen int) {
	b.Helper()
	in := newBenchmarkSendWithReadyLanes(lanes)
	benchmarkSendWriteUDPInstance(b, in, payloadLen)
}

func benchmarkSendWriteUDPInstance(b *testing.B, in *Send, payloadLen int) {
	b.Helper()
	if _, _, ok := in.getOrCreateSessionState(99); !ok {
		b.Fatal("getOrCreateSessionState failed")
	}
	in.activateSession(99)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go drainSendPackets(ctx, in, done)
	defer func() {
		cancel()
		<-done
	}()

	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		packet := packetbuf.Acquire(len(payload))
		copy(packet.Payload, payload)
		packet.SetLen(len(payload))
		if err := in.Write(ctx, packet); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
}

func drainSendPackets(ctx context.Context, in *Send, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-in.Packets():
			payload.Packet.Release()
		}
	}
}
