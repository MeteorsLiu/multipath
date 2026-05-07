package send

import (
	"context"
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

func TestBandwidthProbeEffectiveRateCapsTCP(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	leg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp0"}
	legKey := newPingKey(leg)
	capBps := uint64(50_000_000)

	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:      key,
		capBps:   capBps,
		rateBps:  200_000_000,
		inFlight: true,
		steps:    make(map[uint64]*bandwidthProbeStep),
	}

	step := in.startBandwidthProbeStep(legKey, 1, time.Now())
	if step == nil {
		t.Fatal("nil step")
	}
	if step.rateBps != capBps {
		t.Fatalf("step rateBps = %d, want cap %d", step.rateBps, capBps)
	}

	round := in.startBandwidthProbeRound(key, leg, legKey, 1, time.Now())
	if round == nil {
		t.Fatal("nil round")
	}
	if round.rateBps != capBps {
		t.Fatalf("round rateBps = %d, want cap %d", round.rateBps, capBps)
	}
	if got := probeFrameCount(capBps, bandwidthProbeFrameBytes(bandwidthProbeTCPPayloadSize)); round.count != got {
		t.Fatalf("round count = %d, want count for capped rate %d", round.count, got)
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

func TestBandwidthProbeServerReadyNil(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	if !in.isBandwidthProbeServerReady(key) {
		t.Fatal("nil map should always return true")
	}

	in.markBandwidthProbeDone(99, 2)
	if !in.isBandwidthProbeServerReady(key) {
		t.Fatal("mark on disabled server-ready gate should keep all lanes ready")
	}
	if !in.isBandwidthProbeServerReady(laneKey{sessionID: 99, laneID: 3}) {
		t.Fatal("disabled server-ready gate should not become lane-scoped after mark")
	}
}

func TestBandwidthProbeServerReadyMarked(t *testing.T) {
	in := New()
	in.EnableBandwidthProbeServerReady()
	key := laneKey{sessionID: 99, laneID: 1}

	in.markBandwidthProbeDone(99, 1)

	if !in.isBandwidthProbeServerReady(key) {
		t.Fatal("marked lane should return true")
	}
	other := laneKey{sessionID: 99, laneID: 2}
	if in.isBandwidthProbeServerReady(other) {
		t.Fatal("unmarked lane should return false")
	}
}

func TestBandwidthProbeCompletesZeroAckLaneAndAdvances(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key1 := laneKey{sessionID: 99, laneID: 1}
	lane1 := newLaneRuntime(1, 1)
	leg1 := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp1",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:10001"),
	}
	lane1.bindLeg(leg1)
	in.lanes[key1] = lane1

	key2 := laneKey{sessionID: 99, laneID: 2}
	lane2 := newLaneRuntime(2, 1)
	leg2 := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp2",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:10002"),
	}
	lane2.bindLeg(leg2)
	in.lanes[key2] = lane2

	in.maybeStartBandwidthProbe(ctx, key1, leg1, time.Now())
	waitForBandwidthProbeComplete(t, in, newPingKey(leg1), 3*time.Second)

	_, udpQ1, _, _ := lane1.legQualities()
	if udpQ1.ProbeSamples != 1 || udpQ1.BandwidthBps != 0 || udpQ1.ProbeLoss != 1 {
		t.Fatalf("lane1 UDP quality = samples=%d bps=%d loss=%.3f, want 1/0/1", udpQ1.ProbeSamples, udpQ1.BandwidthBps, udpQ1.ProbeLoss)
	}

	in.probeBandwidth(ctx, time.Now())

	in.bandwidthMu.Lock()
	state2 := in.bandwidthLegs[newPingKey(leg2)]
	started2 := state2 != nil && state2.inFlight
	in.bandwidthMu.Unlock()
	if !started2 {
		t.Fatal("lane2 bandwidth probe did not start after lane1 zero-ack completion")
	}
}

func TestBandwidthProbeLostLegReleasesNextLane(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps

	key2 := laneKey{sessionID: 99, laneID: 2}
	lane2 := newLaneRuntime(2, 1)
	leg2 := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp2"}
	lane2.bindLeg(leg2)
	in.lanes[key2] = lane2
	in.bandwidthLegs[newPingKey(leg2)] = &bandwidthLegState{
		key:       key2,
		capBps:    bandwidthProbeMinRateBps,
		rateBps:   bandwidthProbeMinRateBps,
		inFlight:  true,
		startedAt: time.Now().Add(-time.Second),
		steps:     make(map[uint64]*bandwidthProbeStep),
	}

	key3 := laneKey{sessionID: 99, laneID: 3}
	lane3 := newLaneRuntime(3, 1)
	leg3 := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp3",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:10003"),
	}
	lane3.bindLeg(leg3)
	in.lanes[key3] = lane3

	in.completeBandwidthProbeLostLeg(key2, leg2, "probe_timeout")

	in.bandwidthMu.Lock()
	state2 := in.bandwidthLegs[newPingKey(leg2)]
	done2 := state2 != nil && state2.complete && !state2.inFlight
	in.bandwidthMu.Unlock()
	if !done2 {
		t.Fatal("lost leg did not complete in-flight bandwidth probe")
	}

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	state3 := in.bandwidthLegs[newPingKey(leg3)]
	started3 := state3 != nil && state3.inFlight
	in.bandwidthMu.Unlock()
	if !started3 {
		t.Fatal("lane3 bandwidth probe did not start after lane2 leg loss")
	}
}

func waitForBandwidthProbeComplete(t *testing.T, in *Send, legKey pingKey, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		in.bandwidthMu.Lock()
		state := in.bandwidthLegs[legKey]
		complete := state != nil && state.complete
		in.bandwidthMu.Unlock()
		if complete {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for bandwidth probe completion")
}
