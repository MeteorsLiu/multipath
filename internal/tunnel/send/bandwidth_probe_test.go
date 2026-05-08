package send

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
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

func TestBandwidthProbeUDPUnderDeliveryRequiresPlateauBelowCap(t *testing.T) {
	if !bandwidthProbeUDPUnderDelivery(100_000_000, 80_000_000, []uint64{40_000_000, 40_000_000, 41_000_000}) {
		t.Fatal("want under-delivery for plateau below cap")
	}
	if bandwidthProbeUDPUnderDelivery(100_000_000, 100_000_000, []uint64{40_000_000, 40_000_000, 41_000_000}) {
		t.Fatal("want no under-delivery once probe rate reached cap")
	}
	if bandwidthProbeUDPUnderDelivery(100_000_000, 80_000_000, []uint64{40_000_000, 50_000_000, 60_000_000}) {
		t.Fatal("want no under-delivery while throughput is still growing")
	}
	if bandwidthProbeUDPUnderDelivery(0, 80_000_000, []uint64{40_000_000, 40_000_000, 41_000_000}) {
		t.Fatal("want no under-delivery without a reference cap")
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

func TestBandwidthProbeTrainBudgetFromCap(t *testing.T) {
	budget := bandwidthProbeTrainBudgetBytes(200_000_000)
	want := uint64(200_000_000) * uint64(bandwidthProbeWindow) / uint64(time.Second) / 8
	if budget != want {
		t.Fatalf("budget = %d, want %d", budget, want)
	}
}

func TestBandwidthProbeTrainBudgetForTCPReferenceAllowsRamp(t *testing.T) {
	budget := bandwidthProbeTCPReferenceTrainBudgetBytes()
	min := bandwidthProbeTrainBudgetBytes(200_000_000)
	if budget < min {
		t.Fatalf("tcp reference budget = %d, want at least %d", budget, min)
	}
}

func TestBandwidthProbeRoundCarriesTrainBudget(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	leg := udpLeg()
	legKey := newPingKey(leg)
	budget := uint64(10_000)

	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:                 key,
		rateBps:             bandwidthProbeMinRateBps,
		capBps:              bandwidthProbeMinRateBps,
		inFlight:            true,
		trainID:             42,
		trainBytesTotal:     budget,
		trainBytesRemaining: budget,
		steps:               make(map[uint64]*bandwidthProbeStep),
	}

	round := in.startBandwidthProbeRound(key, leg, legKey, 1, time.Now())
	if round == nil {
		t.Fatal("nil round")
	}
	if round.trainID != 42 || round.trainBytesTotal != budget || round.trainBytesRemaining != budget {
		t.Fatalf("round train fields = id=%d total=%d remaining=%d, want 42/%d/%d", round.trainID, round.trainBytesTotal, round.trainBytesRemaining, budget, budget)
	}
}

func TestBandwidthProbeConsumeTrainBudget(t *testing.T) {
	remaining, last := bandwidthProbeConsumeBudget(1000, 300)
	if remaining != 700 || last {
		t.Fatalf("first consume = (%d,%t), want (700,false)", remaining, last)
	}
	remaining, last = bandwidthProbeConsumeBudget(200, 300)
	if remaining != 0 || !last {
		t.Fatalf("last consume = (%d,%t), want (0,true)", remaining, last)
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

func TestBandwidthProbeGateClientWaitsForRemoteBeforeNextLane(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	key1 := laneKey{sessionID: 99, laneID: 1}
	key2 := laneKey{sessionID: 99, laneID: 2}

	in.advanceBandwidthProbeGateAfterLocal(key1, transport.KindUDP)
	if in.bandwidthProbeGateAllowsLocal(key2, transport.KindUDP) {
		t.Fatal("client gate allowed lane2 before remote lane1 completion")
	}
	if !in.bandwidthProbeGateAllowsRemote(key1, transport.KindUDP) {
		t.Fatal("client gate should wait for remote lane1")
	}
}

func TestBandwidthProbeGateAdvancesAfterRemoteCompletion(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	key1 := laneKey{sessionID: 99, laneID: 1}
	key2 := laneKey{sessionID: 99, laneID: 2}

	in.advanceBandwidthProbeGateAfterLocal(key1, transport.KindUDP)
	in.advanceBandwidthProbeGateAfterRemote(key1, transport.KindUDP)
	if !in.bandwidthProbeGateAllowsLocal(key2, transport.KindUDP) {
		t.Fatal("client gate did not advance to local lane2")
	}
}

func TestBandwidthProbeGateServerStartsRemotePhase(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	in.setBandwidthProbeGateMode(false)
	key1 := laneKey{sessionID: 99, laneID: 1}
	if in.bandwidthProbeGateAllowsLocal(key1, transport.KindUDP) {
		t.Fatal("server should not start local train before client train")
	}
	if !in.bandwidthProbeGateAllowsRemote(key1, transport.KindUDP) {
		t.Fatal("server should accept remote lane1 train first")
	}
}

func TestBandwidthProbeCandidateWithCapSkipsTCP(t *testing.T) {
	in := New()
	in.bandwidthProbeCapBps = 200_000_000
	key := laneKey{sessionID: 99, laneID: 1}
	lane := newLaneRuntime(1, 1)
	udp := udpLeg()
	tcp := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp0"}
	lane.bindLeg(udp)
	lane.bindLeg(tcp)

	leg, ok := in.bandwidthProbeCandidate(key, lane, udp, LegQuality{Active: true}, tcp, LegQuality{Active: true})
	if !ok || leg.Kind != transport.KindUDP {
		t.Fatalf("candidate = (%s,%t), want UDP", debugLeg(leg), ok)
	}
}

func TestBandwidthProbeCappedUDPDecisionDoesNotWaitForTCPReference(t *testing.T) {
	in := New()
	in.bandwidthProbeCapBps = 200_000_000
	key := laneKey{sessionID: 99, laneID: 1}
	lane := newLaneRuntime(1, 1)
	udp := udpLeg()
	tcp := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp0"}
	lane.bindLeg(udp)
	lane.bindLeg(tcp)
	in.lanes[key] = lane

	lane.recordBandwidthSampleWithReference(transport.KindUDP, 80_000_000, bandwidthProbeLossThreshold, in.bandwidthProbeCapBps)

	_, udpQ, _, tcpQ := lane.legQualities()
	if udpQ.ProbeSamples != 1 {
		t.Fatalf("udp probe samples = %d, want 1", udpQ.ProbeSamples)
	}
	if tcpQ.ProbeSamples != 0 {
		t.Fatalf("tcp probe samples = %d, want 0", tcpQ.ProbeSamples)
	}
	if !udpQ.BandwidthQoSLimited || !udpQ.BandwidthPreferTCP {
		t.Fatalf("capped UDP QoS = limited %t prefer_tcp %t, want both true", udpQ.BandwidthQoSLimited, udpQ.BandwidthPreferTCP)
	}
	tcpQ.DeliveryRate = 1
	useUDP, ok := in.legSelector(key.sessionID).Pick(udpQ, tcpQ)
	if !ok || useUDP {
		t.Fatalf("selector = useUDP %t ok %t, want TCP selected", useUDP, ok)
	}
}

func TestBandwidthProbeCandidateNoCapRunsTCPReferenceFirst(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	lane := newLaneRuntime(1, 1)
	udp := udpLeg()
	tcp := tcpLeg()
	lane.bindLeg(udp)
	lane.bindLeg(tcp)

	leg, ok := in.bandwidthProbeCandidate(key, lane, udp, LegQuality{Active: true}, tcp, LegQuality{Active: true})
	if !ok || leg.Kind != transport.KindTCP {
		t.Fatalf("candidate = (%s,%t), want TCP reference", debugLeg(leg), ok)
	}
}

func TestBandwidthProbeUDPAfterTCPReferenceUsesReferenceCap(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	leg := udpLeg()
	legKey := newPingKey(leg)
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(leg)
	lane.recordBandwidthSample(transport.KindTCP, 300_000_000, 0)
	in.lanes[key] = lane

	in.maybeStartBandwidthProbe(context.Background(), key, leg, time.Now())

	in.bandwidthMu.Lock()
	state := in.bandwidthLegs[legKey]
	var capBps uint64
	var trainBytesTotal uint64
	if state != nil {
		capBps = state.capBps
		trainBytesTotal = state.trainBytesTotal
		state.complete = true
		state.inFlight = false
	}
	in.bandwidthMu.Unlock()
	if capBps != 300_000_000 {
		t.Fatalf("UDP capBps = %d, want TCP reference", capBps)
	}
	if want := bandwidthProbeTrainBudgetBytes(300_000_000); trainBytesTotal != want {
		t.Fatalf("UDP train budget = %d, want %d", trainBytesTotal, want)
	}
}

func TestBandwidthProbeCompletesZeroAckLaneWaitsForRemote(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps

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

	legKey1 := newPingKey(leg1)
	in.bandwidthLegs[legKey1] = &bandwidthLegState{
		key:         key1,
		capBps:      bandwidthProbeMinRateBps,
		rateBps:     bandwidthProbeMinRateBps,
		inFlight:    true,
		startedAt:   time.Now().Add(-time.Second),
		endedAt:     time.Now(),
		sentFrames:  10,
		ackedFrames: 0,
		ackedBytes:  0,
		steps:       make(map[uint64]*bandwidthProbeStep),
	}
	if completed := in.completeBandwidthProbeTrain(key1, leg1, legKey1); !completed {
		t.Fatal("lane1 bandwidth probe did not complete")
	}
	in.advanceBandwidthProbeGateAfterLocal(key1, transport.KindUDP)

	in.probeBandwidth(context.Background(), time.Now())
	in.bandwidthMu.Lock()
	state2 := in.bandwidthLegs[newPingKey(leg2)]
	started2 := state2 != nil && state2.inFlight
	in.bandwidthMu.Unlock()
	if started2 {
		t.Fatal("lane2 bandwidth probe started before remote lane1 completion")
	}

	in.advanceBandwidthProbeGateAfterRemote(key1, transport.KindUDP)
	in.probeBandwidth(context.Background(), time.Now())

	waitForBandwidthProbeStarted(t, in, newPingKey(leg2), time.Second)
}

func TestBandwidthProbeLostLegWaitsForRemoteBeforeNextLane(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	in.bandwidthGates[99] = bandwidthProbeGate{
		sessionID:  99,
		laneID:     2,
		legKind:    transport.KindUDP,
		phase:      bandwidthProbePhaseLocal,
		localFirst: true,
	}

	key2 := laneKey{sessionID: 99, laneID: 2}
	lane2 := newLaneRuntime(2, 1)
	leg2 := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp2",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:10002"),
	}
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
	if started3 {
		t.Fatal("lane3 bandwidth probe started before remote lane2 completion")
	}

	in.advanceBandwidthProbeGateAfterRemote(key2, transport.KindUDP)
	in.probeBandwidth(context.Background(), time.Now())

	waitForBandwidthProbeStarted(t, in, newPingKey(leg3), time.Second)
}

func TestBandwidthProbeTrainDoesNotCompleteAfterLostLeg(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 2}
	leg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp2"}
	legKey := newPingKey(leg)
	lane := newLaneRuntime(2, 1)
	lane.bindLeg(leg)
	in.lanes[key] = lane
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:         key,
		rateBps:     bandwidthProbeMinRateBps,
		inFlight:    true,
		startedAt:   time.Now().Add(-time.Second),
		sentFrames:  10,
		ackedFrames: 5,
		ackedBytes:  2048,
		steps:       make(map[uint64]*bandwidthProbeStep),
		stepOrder:   []uint64{1},
	}

	in.completeBandwidthProbeLostLeg(key, leg, "probe_timeout")
	if completed := in.completeBandwidthProbeTrain(key, leg, legKey); completed {
		t.Fatal("completeBandwidthProbeTrain completed after lost leg")
	}

	_, _, _, tcpQ := lane.legQualities()
	if tcpQ.ProbeSamples != 1 {
		t.Fatalf("tcp probe samples = %d, want 1", tcpQ.ProbeSamples)
	}
}

func TestReceiveBandwidthProbeRemainingZeroAdvancesGate(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	in.setBandwidthProbeGateMode(false)
	if _, _, ok := in.getOrCreateSessionState(99); !ok {
		t.Fatal("failed to create session state")
	}
	key := laneKey{sessionID: 99, laneID: 1}
	leg := udpLeg()
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(leg)
	in.lanes[key] = lane

	body := protocol.BandwidthProbeBody{
		TrainID:             7,
		ProbeID:             8,
		Seq:                 0,
		Count:               1,
		SendMS:              1,
		TrainBytesTotal:     100,
		TrainBytesRemaining: 0,
		Payload:             []byte("x"),
	}
	if err := in.receiveBandwidthProbe(context.Background(), 99, 1, leg, body); err != nil {
		t.Fatalf("receiveBandwidthProbe failed: %v", err)
	}
	if !in.bandwidthProbeGateAllowsLocal(key, transport.KindUDP) {
		t.Fatal("server gate did not advance to local after remote completion")
	}
}

func TestRemoteTrainIdleTimeoutDoesNotMarkLaneDown(t *testing.T) {
	in := New(Config{ProbeTimeout: time.Second})
	key := laneKey{sessionID: 99, laneID: 1}
	leg := udpLeg()
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(leg)
	in.lanes[key] = lane

	timeout := in.remoteBandwidthTrainIdleTimeout(key, leg)
	if timeout <= 0 {
		t.Fatalf("timeout = %s, want positive", timeout)
	}
	in.completeRemoteBandwidthProbeTrain(key, leg, 7, "idle_timeout")
	udpLeg, udpQ, _, _ := lane.legQualities()
	if !udpQ.Active || newPingKey(udpLeg) != newPingKey(leg) {
		t.Fatal("remote train timeout should not mark UDP leg down")
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

func waitForBandwidthProbeStarted(t *testing.T, in *Send, legKey pingKey, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		in.bandwidthMu.Lock()
		state := in.bandwidthLegs[legKey]
		started := state != nil && state.inFlight
		in.bandwidthMu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for bandwidth probe start")
}
