package send

import (
	"net"
	"testing"

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
