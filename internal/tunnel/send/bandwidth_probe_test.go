package send

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestBandwidthProbeStartsOneLegAtATime(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)

	udp1 := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp1", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234")}
	tcp1 := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp1"}
	udp2 := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp2", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1235")}
	tcp2 := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp2"}

	lane1 := newLaneRuntime(1, 1)
	lane1.bindLeg(udp1)
	lane1.bindLeg(tcp1)
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = lane1

	lane2 := newLaneRuntime(2, 1)
	lane2.bindLeg(udp2)
	lane2.bindLeg(tcp2)
	in.lanes[laneKey{sessionID: 99, laneID: 2}] = lane2

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()

	if len(in.bandwidthLegs) != 1 {
		t.Fatalf("bandwidth legs = %d, want 1", len(in.bandwidthLegs))
	}
	state := in.bandwidthLegs[newPingKey(udp1)]
	if state == nil || !state.inFlight {
		t.Fatalf("UDP leg state not in flight: %+v", state)
	}
	if state.key.laneID != 1 {
		t.Fatalf("started on lane %d, want lane 1", state.key.laneID)
	}
	if _, ok := in.bandwidthLegs[newPingKey(tcp1)]; ok {
		t.Fatal("TCP probe started while UDP probe is still pending")
	}
}

func TestBandwidthProbeCanBeDisabledByEnv(t *testing.T) {
	t.Setenv("MULTIPATH_DISABLE_BW_PROBE", "1")
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)

	udp := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp1", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234")}
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(udp)
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = lane

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()
	if len(in.bandwidthLegs) != 0 {
		t.Fatalf("bandwidth legs = %d, want none when disabled", len(in.bandwidthLegs))
	}
}

func TestBandwidthProbeEnabledByDefaultAfterEnvDisabled(t *testing.T) {
	t.Setenv("MULTIPATH_DISABLE_BW_PROBE", "1")
	_ = New()
	if err := os.Unsetenv("MULTIPATH_DISABLE_BW_PROBE"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	in := New()
	if !in.bandwidthProbe {
		t.Fatal("bandwidth probe disabled after env was unset")
	}
}

func TestBandwidthProbeStartsTCPAfterUDPCompletes(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)

	key := laneKey{sessionID: 99, laneID: 1}
	udp := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp1", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234")}
	tcp := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp1"}

	lane := newLaneRuntime(1, 1)
	lane.bindLeg(udp)
	lane.bindLeg(tcp)
	in.lanes[key] = lane

	in.bandwidthLegs[newPingKey(udp)] = &bandwidthLegState{key: key, complete: true}

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()

	state := in.bandwidthLegs[newPingKey(tcp)]
	if state == nil || !state.inFlight || state.key != key {
		t.Fatalf("TCP leg state = %+v, want in-flight lane 1 probe", state)
	}
}

func TestBandwidthProbeStartsUDPWhenKnownLegIsDegraded(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)

	key := laneKey{sessionID: 99, laneID: 1}
	udp := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp1", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234")}
	tcp := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp1"}

	lane := newLaneRuntime(1, 1)
	lane.bindLeg(udp)
	lane.bindLeg(tcp)
	lane.markUDPNotReady()
	in.lanes[key] = lane

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()

	state := in.bandwidthLegs[newPingKey(udp)]
	if state == nil || !state.inFlight || state.key != key {
		t.Fatalf("UDP leg state = %+v, want in-flight degraded UDP probe", state)
	}
	if _, ok := in.bandwidthLegs[newPingKey(tcp)]; ok {
		t.Fatal("TCP probe started before degraded UDP probe completed")
	}
}

func TestBandwidthProbeDoesNotStartAnotherLegWhileInFlight(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)

	key1 := laneKey{sessionID: 99, laneID: 1}
	key2 := laneKey{sessionID: 99, laneID: 2}
	udp1 := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp1", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234")}
	tcp1 := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp1"}
	udp2 := transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp2", RemoteAddr: mustUDPAddr(t, "127.0.0.1:1235")}

	lane1 := newLaneRuntime(1, 1)
	lane1.bindLeg(udp1)
	lane1.bindLeg(tcp1)
	in.lanes[key1] = lane1

	lane2 := newLaneRuntime(2, 1)
	lane2.bindLeg(udp2)
	in.lanes[key2] = lane2

	in.bandwidthLegs[newPingKey(udp1)] = &bandwidthLegState{key: key1, inFlight: true}

	in.probeBandwidth(context.Background(), time.Now())

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()
	if len(in.bandwidthLegs) != 1 {
		t.Fatalf("bandwidth legs = %d, want only the existing in-flight leg", len(in.bandwidthLegs))
	}
	if _, ok := in.bandwidthLegs[newPingKey(tcp1)]; ok {
		t.Fatal("started TCP probe while UDP probe is in flight")
	}
	if _, ok := in.bandwidthLegs[newPingKey(udp2)]; ok {
		t.Fatal("started another lane probe while a probe is in flight")
	}
}

func TestSendReceiveBandwidthProbeRepliesWithBitmap(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(leg)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	if err := in.receiveBandwidthProbe(context.Background(), 99, 3, leg, protocol.BandwidthProbeBody{
		ProbeID: 7,
		Seq:     0,
		Count:   2,
		SendMS:  1000,
		Payload: []byte("probe"),
	}); err != nil {
		t.Fatalf("receiveBandwidthProbe seq0 failed: %v", err)
	}
	select {
	case payload := <-in.Packets():
		payload.Packet.Release()
		t.Fatal("unexpected BW_PROBE_ACK before chunk boundary")
	default:
	}

	if err := in.receiveBandwidthProbe(context.Background(), 99, 3, leg, protocol.BandwidthProbeBody{
		ProbeID: 7,
		Seq:     1,
		Count:   2,
		SendMS:  1001,
		Payload: []byte("probe"),
	}); err != nil {
		t.Fatalf("receiveBandwidthProbe seq1 failed: %v", err)
	}
	second := readSendPayload(t, in)
	defer second.Packet.Release()
	secondFrame, err := protocol.Decode(second.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode second ack: %v", err)
	}
	secondAck := secondFrame.Body.(protocol.BandwidthProbeAckBody)
	if secondAck.Received != 0x03 {
		t.Fatalf("second received = %#x, want 0x03", secondAck.Received)
	}
}

func TestSendBandwidthProbeAckUpdatesLaneQuality(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)
	key := laneKey{sessionID: 99, laneID: 3}
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(udpLeg)
	in.lanes[key] = lane

	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
		startedAt:   time.Now().Add(-bandwidthProbeWindow),
		endedAt:     time.Now(),
		sentFrames:  4,
	}
	in.bandwidthPending[7] = &bandwidthProbeRound{
		key:          key,
		leg:          udpLeg,
		legKey:       legKey,
		probeID:      7,
		count:        4,
		payloadBytes: 1000,
	}

	if err := in.receiveBandwidthProbeAck(99, 3, udpLeg, protocol.BandwidthProbeAckBody{
		ProbeID:   7,
		Count:     4,
		Received:  0x03,
		FirstRXMS: 1000,
		LastRXMS:  1001,
	}); err != nil {
		t.Fatalf("receiveBandwidthProbeAck failed: %v", err)
	}
	in.completeBandwidthProbeTrain(key, udpLeg, legKey)

	_, udpQ, _, _ := lane.legQualities()
	if udpQ.BandwidthBps == 0 {
		t.Fatal("udp bandwidth EWMA was not updated")
	}
	if udpQ.ProbeLoss != 0.5 {
		t.Fatalf("udp probe loss = %.2f, want 0.50", udpQ.ProbeLoss)
	}
	if state := in.bandwidthLegs[legKey]; state == nil || !state.complete {
		t.Fatalf("probe complete = %v, want true after window elapsed", state != nil && state.complete)
	}
}

func TestBandwidthProbeCompletionUsesSustainedWindowSample(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)
	key := laneKey{sessionID: 99, laneID: 3}
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(udpLeg)
	in.lanes[key] = lane

	legKey := newPingKey(udpLeg)
	startedAt := time.Now().Add(-10 * time.Second)
	endedAt := startedAt.Add(10 * time.Second)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
		startedAt:   startedAt,
		endedAt:     endedAt,
		ackedBytes:  1_000_000,
		sentFrames:  100,
		ackedFrames: 100,
		maxStepBps:  800_000_000,
	}

	in.completeBandwidthProbeTrain(key, udpLeg, legKey)

	_, udpQ, _, _ := lane.legQualities()
	if udpQ.BandwidthBps != 800_000 {
		t.Fatalf("udp bandwidth = %d, want sustained 800000bps", udpQ.BandwidthBps)
	}
}

func TestBandwidthProbeAckAttributesBytesToOriginalStep(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
		steps:       make(map[uint64]*bandwidthProbeStep),
	}

	step1Start := time.Now()
	in.startBandwidthProbeStep(legKey, step1Start)
	round := in.startBandwidthProbeRound(key, udpLeg, legKey, step1Start)
	if round == nil {
		t.Fatal("missing first probe round")
	}
	if round.stepID != 1 {
		t.Fatalf("first round stepID = %d, want 1", round.stepID)
	}
	in.finishBandwidthProbeStep(legKey, step1Start.Add(time.Second))

	step2Start := step1Start.Add(time.Second)
	in.startBandwidthProbeStep(legKey, step2Start)
	if err := in.receiveBandwidthProbeAck(99, 3, udpLeg, protocol.BandwidthProbeAckBody{
		ProbeID:  round.probeID,
		Count:    round.count,
		Received: 0x03,
	}); err != nil {
		t.Fatalf("receiveBandwidthProbeAck failed: %v", err)
	}
	in.finishBandwidthProbeStep(legKey, step2Start.Add(time.Second))

	in.bandwidthMu.Lock()
	defer in.bandwidthMu.Unlock()
	state := in.bandwidthLegs[legKey]
	if state == nil {
		t.Fatal("missing bandwidth leg state")
	}
	step1 := state.steps[round.stepID]
	if step1 == nil {
		t.Fatal("missing step 1")
	}
	wantAckedBytes := uint64(2 * round.payloadBytes)
	if step1.ackedBytes != wantAckedBytes {
		t.Fatalf("step 1 ackedBytes = %d, want %d", step1.ackedBytes, wantAckedBytes)
	}
	step2 := state.steps[state.currentStepID]
	if step2 == nil {
		t.Fatal("missing step 2")
	}
	if step2.ackedBytes != 0 {
		t.Fatalf("step 2 ackedBytes = %d, want 0", step2.ackedBytes)
	}
	if state.lastStepBps != 0 {
		t.Fatalf("last step bps = %d, want 0 for delayed ack attributed to step 1", state.lastStepBps)
	}
	wantMaxStepBps := bandwidthWindowSampleBps(wantAckedBytes, step1Start, step1Start.Add(time.Second))
	if state.maxStepBps != wantMaxStepBps {
		t.Fatalf("max step bps = %d, want %d", state.maxStepBps, wantMaxStepBps)
	}
}

func TestBandwidthProbeRateAdvancesAdditivelyAfterStartup(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
		lastStepBps: 20_000_000,
	}

	in.advanceBandwidthProbeRate(legKey)

	state := in.bandwidthLegs[legKey]
	if state.nextRateBps != 26_000_000 {
		t.Fatalf("next rate = %d, want additive increase to 26Mbps", state.nextRateBps)
	}
}

func TestBandwidthProbeRateAdvanceLocksDynamicCeilingWhenAckBytesStopGrowing(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps:   90_000_000,
		inFlight:      true,
		lastStepBps:   60_000_000,
		lastStepBytes: 4_300_000,
		prevStepBytes: 4_000_000,
	}

	in.advanceBandwidthProbeRate(legKey)

	state := in.bandwidthLegs[legKey]
	if state.rateCeilingBps != 90_000_000 {
		t.Fatalf("rate ceiling = %d, want 90Mbps", state.rateCeilingBps)
	}
	if state.nextRateBps != 90_000_000 {
		t.Fatalf("next rate = %d, want dynamic ceiling", state.nextRateBps)
	}

	state.lastStepBps = 120_000_000
	state.lastStepBytes = 5_000_000
	in.advanceBandwidthProbeRate(legKey)

	if state.nextRateBps != 90_000_000 {
		t.Fatalf("next rate after ceiling = %d, want dynamic ceiling", state.nextRateBps)
	}
}

func TestBandwidthProbeRateUsesAdditiveIncreaseWhenAckBytesGrowEnough(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps:   90_000_000,
		inFlight:      true,
		lastStepBps:   80_000_000,
		lastStepBytes: 4_400_000,
		prevStepBytes: 4_000_000,
	}

	in.advanceBandwidthProbeRate(legKey)

	state := in.bandwidthLegs[legKey]
	if state.rateCeilingBps != 0 {
		t.Fatalf("rate ceiling = %d, want none", state.rateCeilingBps)
	}
	if state.nextRateBps != 100_000_000 {
		t.Fatalf("next rate = %d, want additive increase to 100Mbps", state.nextRateBps)
	}
}

func TestBandwidthProbeRateAdvanceUsesAdditiveStepWhenLossIncreases(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps:   90_000_000,
		inFlight:      true,
		lastStepBps:   100_000_000,
		lastStepBytes: 6_000_000,
		prevStepBytes: 4_000_000,
		prevStepLoss:  0.02,
		lastStepLoss:  0.04,
	}

	in.advanceBandwidthProbeRate(legKey)

	state := in.bandwidthLegs[legKey]
	if state.nextRateBps != 100_000_000 {
		t.Fatalf("next rate = %d, want additive increase to 100Mbps", state.nextRateBps)
	}
	if state.rateCeilingBps != 0 {
		t.Fatalf("dynamic ceiling = %d, want none before ack plateau", state.rateCeilingBps)
	}
}

func TestBandwidthProbeRateAdvanceTreatsFirstLossAsIncrease(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps:   90_000_000,
		inFlight:      true,
		lastStepBps:   100_000_000,
		lastStepBytes: 6_000_000,
		prevStepBytes: 4_000_000,
		prevStepLoss:  0,
		lastStepLoss:  bandwidthProbeLossIncreaseEpsilon + 0.001,
	}

	in.advanceBandwidthProbeRate(legKey)

	state := in.bandwidthLegs[legKey]
	if state.nextRateBps != 100_000_000 {
		t.Fatalf("next rate = %d, want additive increase after first loss", state.nextRateBps)
	}
}

func TestBandwidthProbeLimiterUsesByteRateAndShortBurst(t *testing.T) {
	limiter := newBandwidthProbeLimiter(bandwidthProbeMinRateBps, bandwidthProbeUDPMinPayloadSize)
	if limiter == nil {
		t.Fatal("missing limiter")
	}
	if got := limiter.Burst(); got != 4000 {
		t.Fatalf("burst = %d, want 4000 bytes for 16Mbps over 2ms", got)
	}
	if got := limiter.Limit(); got != 2_000_000 {
		t.Fatalf("limit = %v, want 2000000 bytes/s", got)
	}
}

func TestBandwidthProbeLimiterStateSurvivesUDPPayloadJitter(t *testing.T) {
	state := bandwidthProbeLimiterState{}
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp1",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	firstRound := &bandwidthProbeRound{
		rateBps:      bandwidthProbeMinRateBps,
		payloadBytes: bandwidthProbeUDPMinPayloadSize,
	}
	first := state.forRound(leg, firstRound)
	if first == nil {
		t.Fatal("missing limiter")
	}

	secondRound := &bandwidthProbeRound{
		rateBps:      bandwidthProbeMinRateBps,
		payloadBytes: bandwidthProbeUDPMaxPayloadSize,
	}
	second := state.forRound(leg, secondRound)
	if second != first {
		t.Fatal("limiter was recreated for UDP payload jitter")
	}

	nextRound := &bandwidthProbeRound{
		rateBps:      bandwidthProbeMinRateBps + bandwidthProbeAdditiveStepBps,
		payloadBytes: bandwidthProbeUDPMinPayloadSize,
	}
	next := state.forRound(leg, nextRound)
	if next != first {
		t.Fatal("limiter was recreated after rate changed")
	}
	wantLimit := float64((bandwidthProbeMinRateBps + bandwidthProbeAdditiveStepBps) / 8)
	if got := float64(next.Limit()); got != wantLimit {
		t.Fatalf("limit after rate change = %v, want %v", got, wantLimit)
	}
}

func TestBandwidthProbeUDPPayloadSizeIsJittered(t *testing.T) {
	key := laneKey{sessionID: 99, laneID: 1}
	sawDifferent := false
	first := bandwidthProbeUDPPayloadSize(key, 1, bandwidthProbeMinRateBps)
	for probeID := uint64(1); probeID <= 16; probeID++ {
		got := bandwidthProbeUDPPayloadSize(key, probeID, bandwidthProbeMinRateBps)
		if got < bandwidthProbeUDPMinPayloadSize || got > bandwidthProbeUDPMaxPayloadSize {
			t.Fatalf("payload size = %d, want [%d,%d]", got, bandwidthProbeUDPMinPayloadSize, bandwidthProbeUDPMaxPayloadSize)
		}
		if got != first {
			sawDifferent = true
		}
	}
	if !sawDifferent {
		t.Fatal("payload size did not jitter across probe ids")
	}
}

func TestBandwidthProbePayloadFillIsNotZero(t *testing.T) {
	round := &bandwidthProbeRound{
		key:          laneKey{sessionID: 99, laneID: 1},
		probeID:      7,
		payloadBytes: 128,
		rateBps:      bandwidthProbeMinRateBps,
	}
	payload := make([]byte, round.payloadBytes)
	fillBandwidthProbePayload(payload, round)
	if bytes.Equal(payload, make([]byte, len(payload))) {
		t.Fatal("payload is all zero")
	}
	again := make([]byte, round.payloadBytes)
	fillBandwidthProbePayload(again, round)
	if !bytes.Equal(payload, again) {
		t.Fatal("payload fill is not deterministic")
	}
}

func TestLaneBandwidthQoSClassification(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	lane.recordBandwidthSample(transport.KindUDP, 59_000_000, 0.041)
	lane.recordBandwidthSample(transport.KindTCP, 100_000_000, 0)

	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after UDP probe loss")
	}
	if !udpQ.BandwidthPreferTCP {
		t.Fatal("TCP was not preferred when UDP had loss and TCP bandwidth was materially higher")
	}
}

func TestLaneBandwidthQoSLimitedKeepsUDPWhenTCPGainIsSmall(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	lane.recordBandwidthSample(transport.KindUDP, 59_000_000, 0.041)
	lane.recordBandwidthSample(transport.KindTCP, 81_000_000, 0)

	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after UDP probe loss")
	}
	if udpQ.BandwidthPreferTCP {
		t.Fatal("TCP was preferred without a material bandwidth gain")
	}
}

func TestLaneBandwidthQoSLimitedButKeepsUDPWhenTCPIsSlower(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	lane.recordBandwidthSample(transport.KindUDP, 115_000_000, 0.021)
	lane.recordBandwidthSample(transport.KindTCP, 35_000_000, 0)

	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after UDP probe loss")
	}
	if udpQ.BandwidthPreferTCP {
		t.Fatal("TCP was preferred even though TCP bandwidth was lower than UDP")
	}
}

func TestBandwidthProbeDoesNotUpdateLaneQualityBeforeTrainCompletes(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(udpLeg)
	in.lanes[key] = lane

	legKey := newPingKey(udpLeg)
	state := &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
		startedAt:   time.Now(),
	}
	in.bandwidthLegs[legKey] = state
	in.bandwidthPending[7] = &bandwidthProbeRound{
		key:          key,
		leg:          udpLeg,
		legKey:       legKey,
		probeID:      7,
		count:        4,
		payloadBytes: 1000,
		received:     0x0f,
	}

	_, udpQ, _, _ := lane.legQualities()
	if udpQ.ProbeSamples != 0 || udpQ.BandwidthBps != 0 {
		t.Fatalf("udp quality updated before completion: samples=%d bps=%d", udpQ.ProbeSamples, udpQ.BandwidthBps)
	}
}
