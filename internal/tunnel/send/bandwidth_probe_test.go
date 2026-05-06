package send

import (
	"net"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func udpLeg() transport.LegRef {
	return transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
	}
}

func tcpLeg() transport.LegRef {
	return transport.LegRef{
		Kind:       transport.KindTCP,
		EndpointID: "tcp0",
		RemoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678},
	}
}

func TestBandwidthProbePlateau(t *testing.T) {
	if !bandwidthProbePlateau([]uint64{100, 100, 100}) {
		t.Fatal("want plateau for flat samples")
	}
	if bandwidthProbePlateau([]uint64{100, 110, 120}) {
		t.Fatal("want no plateau for growing samples")
	}
	if bandwidthProbePlateau([]uint64{100, 100}) {
		t.Fatal("want no plateau with 2 samples")
	}
	if bandwidthProbePlateau([]uint64{100, 100, 105}) {
		t.Fatal("want no plateau at growth boundary")
	}
	if !bandwidthProbePlateau([]uint64{100, 100, 104}) {
		t.Fatal("want plateau just below growth threshold")
	}
}

func TestNextBandwidthProbeRateUDPAdaptive(t *testing.T) {
	cap := uint64(100_000_000)

	if got := nextBandwidthProbeRate(25_000_000, cap, false); got != 50_000_000 {
		t.Fatalf("far from cap: got %d, want 50000000", got)
	}
	if got := nextBandwidthProbeRate(50_000_000, cap, false); got != 75_000_000 {
		t.Fatalf("medium gap: got %d, want 75000000", got)
	}
	next := nextBandwidthProbeRate(90_000_000, cap, false)
	if next > cap || next <= 90_000_000 {
		t.Fatalf("close to cap: got %d, want between 90M and %d", next, cap)
	}
	if got := nextBandwidthProbeRate(cap, cap, false); got != cap {
		t.Fatalf("at cap: got %d, want %d", got, cap)
	}
	if got := nextBandwidthProbeRate(120_000_000, cap, false); got != cap {
		t.Fatalf("above cap: got %d, want %d", got, cap)
	}
}

func TestNextBandwidthProbeRateTCPNoCap(t *testing.T) {
	if got := nextBandwidthProbeRate(16_000_000, 0, false); got != 26_000_000 {
		t.Fatalf("TCP no plateau: got %d, want 26000000", got)
	}
	if got := nextBandwidthProbeRate(128_000_000, 0, true); got != 128_000_000 {
		t.Fatalf("TCP plateau: got %d, want 128000000", got)
	}
}

func TestBandwidthProbeStartRate(t *testing.T) {
	if got := bandwidthProbeStartRate(0); got != bandwidthProbeMinRateBps {
		t.Fatalf("no cap: got %d, want %d", got, bandwidthProbeMinRateBps)
	}
	if got := bandwidthProbeStartRate(100_000_000); got != 25_000_000 {
		t.Fatalf("100M cap: got %d, want 25000000", got)
	}
	if got := bandwidthProbeStartRate(50_000_000); got != 16_000_000 {
		t.Fatalf("50M cap (ref/4 < min): got %d, want 16000000", got)
	}
	if got := bandwidthProbeStartRate(4_000_000); got != 4_000_000 {
		t.Fatalf("4M cap (cap < min): got %d, want 4000000", got)
	}
}

func TestBandwidthProbeLimiterFromRate(t *testing.T) {
	limiter := newBandwidthProbeLimiter(bandwidthProbeMinRateBps, bandwidthProbeFrameBytes(bandwidthProbeUDPMinPayloadSize))
	if limiter == nil {
		t.Fatal("missing limiter")
	}
}

func TestBandwidthProbeFrameEncoding(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	leg := udpLeg()
	legKey := newPingKey(leg)

	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:      key,
		rateBps:  bandwidthProbeMinRateBps,
		inFlight: true,
		steps:    make(map[uint64]*bandwidthProbeStep),
	}

	round := in.startBandwidthProbeRound(key, leg, legKey, 1, time.Now())
	if round == nil {
		t.Fatal("nil round")
	}
	if round.count == 0 {
		t.Fatal("round count is 0")
	}
	if round.payloadBytes == 0 {
		t.Fatal("payload bytes is 0")
	}
}

func TestProbeFrameCount(t *testing.T) {
	count := probeFrameCount(16_000_000, bandwidthProbeFrameBytes(1400))
	if count < 2 || count > bandwidthProbeMaxFrames {
		t.Fatalf("count=%d, want [2,%d]", count, bandwidthProbeMaxFrames)
	}
	count = probeFrameCount(1_000_000_000, bandwidthProbeFrameBytes(bandwidthProbeTCPPayloadSize))
	if count != bandwidthProbeMaxFrames {
		t.Fatalf("count=%d, want capped at %d", count, bandwidthProbeMaxFrames)
	}
}
