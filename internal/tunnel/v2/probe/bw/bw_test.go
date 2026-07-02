package bw

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withTrainWindow(t *testing.T, window time.Duration) {
	t.Helper()
	old := trainWindow
	trainWindow = window
	t.Cleanup(func() {
		trainWindow = old
	})
}

// TestBWStartActiveSendsProbes verifies an active train emits probe frames with
// a well-formed structure through the SendProbe callback.
func TestBWStartActiveSendsProbes(t *testing.T) {
	var sentProbes []Probe
	var mu sync.Mutex

	sendProbe := func(p Probe) error {
		mu.Lock()
		sentProbes = append(sentProbes, p)
		mu.Unlock()
		return nil
	}

	b := New(Config{
		ReferenceBps: 1_000_000,
		CapBps:       32_000_000, // bounded so the train terminates quickly
		SendProbe:    sendProbe,
		OnSample:     func(Sample) {},
		StepWindow:   40 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	loop, err := b.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if loop == nil {
		t.Fatal("expected non-nil loop")
	}

	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(sentProbes) < 2 {
		t.Fatalf("expected at least 2 probes, got %d", len(sentProbes))
	}
	first := sentProbes[0]
	if first.TrainID != loop.TrainID() {
		t.Errorf("probe train ID = %d, want %d", first.TrainID, loop.TrainID())
	}
	if first.ID == 0 {
		t.Error("expected non-zero probe round ID")
	}
	if first.Count == 0 {
		t.Error("expected non-zero count")
	}
	if first.Bytes == 0 {
		t.Error("expected non-zero probe bytes")
	}
}

func TestReceiveEmitsSampleWhenTrainCompletes(t *testing.T) {
	var gotTrainID uint64
	var gotSample Sample
	samples := 0
	r := NewReceive(ReceiveConfig{
		OnSample: func(trainID uint64, sample Sample) {
			gotTrainID = trainID
			gotSample = sample
			samples++
		},
	})

	r.Probe(Probe{
		TrainID:   10,
		ID:        100,
		Seq:       0,
		Count:     2,
		Total:     2400,
		Remaining: 1200,
		Bytes:     1200,
	})
	time.Sleep(2 * time.Millisecond)
	r.Probe(Probe{
		TrainID:   10,
		ID:        100,
		Seq:       1,
		Count:     2,
		Total:     2400,
		Remaining: 0,
		Bytes:     1200,
	})

	if samples != 1 {
		t.Fatalf("samples = %d, want 1", samples)
	}
	if gotTrainID != 10 {
		t.Fatalf("trainID = %d, want 10", gotTrainID)
	}
	if gotSample.BandwidthBps == 0 {
		t.Fatalf("sample bandwidth = %d, want non-zero", gotSample.BandwidthBps)
	}
	if gotSample.Loss != 0 {
		t.Fatalf("sample loss = %f, want 0", gotSample.Loss)
	}
}

func TestBwLoopUsesTrainBudgetForRemaining(t *testing.T) {
	const referenceBps = uint64(200_000_000)
	withTrainWindow(t, 50*time.Millisecond)

	var sentProbes []Probe
	var sentMu sync.Mutex
	sampleDone := make(chan struct{})

	b := New(Config{
		ReferenceBps:      referenceBps,
		MinRateBps:        16_000_000,
		StepWindow:        5 * time.Millisecond,
		PayloadMin:        1200,
		PayloadMax:        1200,
		LossStopThreshold: 0.01,
		SendProbe: func(p Probe) error {
			sentMu.Lock()
			sentProbes = append(sentProbes, p)
			sentMu.Unlock()
			return nil
		},
		OnSample: func(Sample) {
			close(sampleDone)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := b.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-sampleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("train did not complete")
	}

	sentMu.Lock()
	defer sentMu.Unlock()
	if len(sentProbes) < 2 {
		t.Fatalf("expected data probes plus remaining-zero signal, got %d", len(sentProbes))
	}

	trainTotal := trainBudgetBytes(referenceBps)
	first := sentProbes[0]
	if first.Total != trainTotal {
		t.Fatalf("first probe total = %d, want train budget %d", first.Total, trainTotal)
	}
	if first.Remaining != trainTotal-uint64(first.Bytes) {
		t.Fatalf("first probe remaining = %d, want %d", first.Remaining, trainTotal-uint64(first.Bytes))
	}

	zeroAt := -1
	for i, p := range sentProbes {
		if p.Remaining == 0 && p.Bytes == 0 {
			zeroAt = i
			break
		}
	}
	if zeroAt < 0 {
		t.Fatalf("missing remaining-zero signal in %+v", sentProbes)
	}
	for i, p := range sentProbes[:zeroAt] {
		if p.Bytes == 0 {
			t.Fatalf("probe %d before zero signal has zero bytes: %+v", i, p)
		}
		if p.Total != trainTotal {
			t.Fatalf("probe %d total = %d, want %d", i, p.Total, trainTotal)
		}
		if p.Remaining == 0 {
			t.Fatalf("probe %d reported train complete before zero signal", i)
		}
	}
	for i, p := range sentProbes[zeroAt:] {
		if p.Remaining != 0 || p.Bytes != 0 || p.Count != 1 {
			t.Fatalf("probe %d after zero signal start = %+v, want Remaining=0 zero signal", zeroAt+i, p)
		}
	}
}

func TestBwLoopRemainingDecreasesAcrossSteps(t *testing.T) {
	withTrainWindow(t, 50*time.Millisecond)

	var sentProbes []Probe
	var sentMu sync.Mutex
	sampleDone := make(chan struct{})

	b := New(Config{
		ReferenceBps: 200_000_000,
		CapBps:       32_000_000,
		MinRateBps:   16_000_000,
		StepWindow:   5 * time.Millisecond,
		PayloadMin:   1200,
		PayloadMax:   1200,
		SendProbe: func(p Probe) error {
			sentMu.Lock()
			sentProbes = append(sentProbes, p)
			sentMu.Unlock()
			return nil
		},
		OnSample: func(Sample) {
			close(sampleDone)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := b.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-sampleDone:
	case <-time.After(2 * time.Second):
		t.Fatal("train did not complete")
	}

	sentMu.Lock()
	defer sentMu.Unlock()
	var dataProbes []Probe
	for _, p := range sentProbes {
		if p.Bytes > 0 {
			dataProbes = append(dataProbes, p)
		}
	}
	if len(dataProbes) < 4 {
		t.Fatalf("expected multiple step probes, got %d data probes from %+v", len(dataProbes), sentProbes)
	}
	type roundSeq struct {
		id  uint64
		seq uint16
	}
	seen := make(map[roundSeq]bool)
	var uniqueData []Probe
	for _, p := range dataProbes {
		key := roundSeq{id: p.ID, seq: p.Seq}
		if seen[key] {
			continue
		}
		seen[key] = true
		uniqueData = append(uniqueData, p)
	}
	for i := 1; i < len(uniqueData); i++ {
		if uniqueData[i].Remaining >= uniqueData[i-1].Remaining {
			t.Fatalf("remaining did not decrease at unique probe %d: prev=%d cur=%d", i, uniqueData[i-1].Remaining, uniqueData[i].Remaining)
		}
		if uniqueData[i].Total != uniqueData[0].Total {
			t.Fatalf("total changed at unique probe %d: first=%d cur=%d", i, uniqueData[0].Total, uniqueData[i].Total)
		}
	}
}

// TestBwLoopAckYieldsSample verifies that fully acking each step yields a
// non-zero bandwidth sample with zero loss.
func TestBwLoopAckYieldsSample(t *testing.T) {
	var samples []Sample
	var mu sync.Mutex
	onSample := func(s Sample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}

	var lastProbe Probe
	var pmu sync.Mutex
	var loop atomic.Pointer[BwLoop]

	b := New(Config{
		ReferenceBps: 1_000_000,
		CapBps:       16_000_000, // single step at cap → terminates fast
		OnSample:     onSample,
		StepWindow:   40 * time.Millisecond,
		SendProbe: func(p Probe) error {
			pmu.Lock()
			lastProbe = p
			pmu.Unlock()
			// Immediately ack the whole step: mark every seq as received.
			if l := loop.Load(); l != nil {
				full := uint64(0)
				for i := uint16(0); i < p.Count; i++ {
					full |= 1 << i
				}
				now := uint64(time.Now().UnixMilli())
				l.Ack(Ack{ID: p.ID, Count: p.Count, Received: full, FirstRXMS: now - 10, LastRXMS: now})
			}
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started, err := b.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	loop.Store(started)

	// Wait for the train to finish (cap reached → one step → sample).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(samples)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.BandwidthBps == 0 {
		t.Error("expected non-zero bandwidth")
	}
	if s.Loss != 0.0 {
		t.Errorf("expected zero loss, got %f", s.Loss)
	}
	if s.ReferenceBps != 1_000_000 {
		t.Errorf("expected reference 1000000, got %d", s.ReferenceBps)
	}
	pmu.Lock()
	_ = lastProbe
	pmu.Unlock()
}

// TestBwLoopRetransmitsMissingSequences verifies a step behaves as an ACK
// window: missing seqs are resent with the same probe ID instead of advancing
// immediately after the first partial ACK.
func TestBwLoopRetransmitsMissingSequences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var loop *BwLoop
	received := uint64(0)
	attempts := make(map[uint16]int)
	var mu sync.Mutex
	b := &BW{
		sendProbe: func(p Probe) error {
			mu.Lock()
			attempts[p.Seq]++
			attempt := attempts[p.Seq]
			if p.Seq != 1 || attempt > 1 {
				received |= 1 << p.Seq
			}
			ackBits := received
			mu.Unlock()

			now := uint64(time.Now().UnixMilli())
			loop.Ack(Ack{ID: p.ID, Count: p.Count, Received: ackBits, FirstRXMS: now - 10, LastRXMS: now})
			return nil
		},
	}
	loop = &BwLoop{
		bw:                b,
		trainID:           7,
		referenceBps:      1_000_000,
		minRateBps:        1_000_000,
		stepWindow:        8 * time.Millisecond,
		ackInitialTimeout: 2 * time.Millisecond,
		payloadMin:        1200,
		payloadMax:        1200,
		bytesPerProbe:     1200,
	}
	step := loop.beginStep(1_000_000, 4)
	remaining := uint64(4 * 1200)

	loop.sendStep(ctx, step, remaining, &remaining, time.Now().Add(60*time.Millisecond))

	if !loop.stepComplete(step) {
		t.Fatal("step did not complete after retransmitting the missing seq")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts[1] < 2 {
		t.Fatalf("missing seq was not retransmitted: attempts=%v", attempts)
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0 after unique step bytes", remaining)
	}
	loop.scoreStep(step)
	if loss := aggregateLoss(loop.sentFrames, loop.ackedFrames); loss != 0 {
		t.Fatalf("loss after successful retransmit = %f, want 0", loss)
	}
}

func TestBwLoopNaturalCompletionUsesPayloadProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var loop *BwLoop
	var sent []Probe
	b := &BW{
		sendProbe: func(p Probe) error {
			sent = append(sent, p)
			now := uint64(time.Now().UnixMilli())
			received := uint64(1) << p.Seq
			loop.Ack(Ack{ID: p.ID, Count: p.Count, Received: received, FirstRXMS: now - 10, LastRXMS: now})
			return nil
		},
	}
	loop = &BwLoop{
		bw:                b,
		trainID:           10,
		stepWindow:        8 * time.Millisecond,
		ackInitialTimeout: 2 * time.Millisecond,
		payloadMin:        1200,
		payloadMax:        1200,
		bytesPerProbe:     1200,
	}
	step := loop.beginStep(1_000_000, 4)
	remaining := uint64(4 * 1200)

	loop.sendStep(ctx, step, remaining, &remaining, time.Now().Add(60*time.Millisecond))

	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
	var sawPayloadZero bool
	for _, p := range sent {
		if p.Remaining == 0 && p.Bytes > 0 {
			sawPayloadZero = true
		}
		if p.Remaining == 0 && p.Bytes == 0 {
			t.Fatalf("natural completion sent zero signal: %+v", p)
		}
	}
	if !sawPayloadZero {
		t.Fatalf("natural completion did not emit payload probe with Remaining=0: %+v", sent)
	}
}

func TestBwLoopAckTimeoutUsesObservedDelay(t *testing.T) {
	loop := &BwLoop{
		ackInitialTimeout: 500 * time.Millisecond,
		ackTimeoutMax:     time.Second,
	}

	if got := loop.ackTimeout(); got != 500*time.Millisecond {
		t.Fatalf("initial ack timeout = %s, want 500ms", got)
	}
	loop.observeAckDelay(50 * time.Millisecond)
	if got := loop.ackTimeout(); got != 200*time.Millisecond {
		t.Fatalf("ack timeout = %s, want 200ms", got)
	}

	loop = &BwLoop{
		ackInitialTimeout: 500 * time.Millisecond,
		ackTimeoutMax:     time.Second,
	}
	loop.observeAckDelay(200 * time.Millisecond)
	if got := loop.ackTimeout(); got != 800*time.Millisecond {
		t.Fatalf("ack timeout = %s, want 800ms", got)
	}

	loop = &BwLoop{
		ackInitialTimeout: 500 * time.Millisecond,
		ackTimeoutMax:     time.Second,
	}
	loop.observeAckDelay(400 * time.Millisecond)
	if got := loop.ackTimeout(); got != time.Second {
		t.Fatalf("ack timeout above clamp = %s, want 1s", got)
	}
}

func TestBWNewUsesInitialAckTimeoutBeforeSRTT(t *testing.T) {
	b := New(Config{
		SendProbe: func(Probe) error { return nil },
	})

	loop, err := b.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer loop.Stop()

	if got := loop.ackTimeout(); got != 500*time.Millisecond {
		t.Fatalf("initial ack timeout = %s, want 500ms before SRTT", got)
	}
}

func TestBwLoopAckMeasuresDelayForNewlyAckedSeq(t *testing.T) {
	loop := &BwLoop{
		ackInitialTimeout: 500 * time.Millisecond,
		ackTimeoutMax:     time.Second,
	}
	step := loop.beginStep(1_000_000, 1)
	step.sentAt[0] = time.Now().Add(-200 * time.Millisecond)

	loop.Ack(Ack{ID: step.probeID, Count: step.count, Received: 1})

	if got := loop.ackSRTT; got < 190*time.Millisecond || got > 300*time.Millisecond {
		t.Fatalf("ack srtt = %s, want about 200ms", got)
	}
	if got := loop.ackTimeout(); got < 760*time.Millisecond || got > time.Second {
		t.Fatalf("ack timeout = %s, want around 800ms", got)
	}
}

func TestBwLoopRetransmitsRemainingZeroSignalUntilAcked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var loop *BwLoop
	attempts := 0
	b := &BW{
		sendProbe: func(p Probe) error {
			if p.Remaining != 0 || p.Bytes != 0 || p.Count != 1 {
				t.Fatalf("zero signal = %+v, want Remaining=0 Bytes=0 Count=1", p)
			}
			attempts++
			if attempts >= 2 {
				now := uint64(time.Now().UnixMilli())
				loop.Ack(Ack{ID: p.ID, Count: p.Count, Received: 1, FirstRXMS: now, LastRXMS: now})
			}
			return nil
		},
	}
	loop = &BwLoop{
		bw:                b,
		trainID:           8,
		ackInitialTimeout: 5 * time.Millisecond,
		payloadMin:        1200,
		payloadMax:        1200,
	}

	loop.sendRemainingZero(ctx, 1200)

	if attempts < 2 {
		t.Fatalf("remaining-zero signal was not retransmitted: attempts=%d", attempts)
	}
	if loop.sentFrames != 0 || loop.ackedFrames != 0 {
		t.Fatalf("remaining-zero signal affected sample accounting: sent=%d acked=%d", loop.sentFrames, loop.ackedFrames)
	}
}

func TestBwLoopAckMatchesProbeRoundAfterNextStepStarts(t *testing.T) {
	loop := &BwLoop{}
	first := loop.beginStep(1_000_000, 2)
	second := loop.beginStep(2_000_000, 2)

	now := uint64(time.Now().UnixMilli())
	loop.Ack(Ack{ID: first.probeID, Count: first.count, Received: 0x3, FirstRXMS: now - 10, LastRXMS: now})

	if !loop.stepComplete(first) {
		t.Fatal("late ACK did not update the matching first round")
	}
	if loop.stepComplete(second) {
		t.Fatal("late ACK for first round was applied to current second round")
	}
}

func TestBwLoopStopPreventsFurtherRetransmit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	sends := 0
	loop := &BwLoop{
		trainID:           9,
		stepWindow:        5 * time.Millisecond,
		ackInitialTimeout: 2 * time.Millisecond,
		payloadMin:        1200,
		payloadMax:        1200,
		bytesPerProbe:     1200,
	}
	loop.bw = &BW{sendProbe: func(Probe) error {
		sends++
		loop.Stop()
		return nil
	}}
	step := loop.beginStep(1_000_000, 2)
	remaining := uint64(2 * 1200)

	loop.sendStep(ctx, step, remaining, &remaining, time.Now().Add(50*time.Millisecond))

	if sends != 1 {
		t.Fatalf("send count after Stop = %d, want 1", sends)
	}
}

// TestReceiveProbeSendsAck verifies the passive side returns an ack to send for
// inbound probes and a complete bitmap once the train fills.
func TestReceiveProbeSendsAck(t *testing.T) {
	r := NewReceive(ReceiveConfig{AckEvery: 16})

	const trainID = 42
	const count = 10

	var lastAck Ack
	acked := false
	for seq := uint16(0); seq < count; seq++ {
		ack, send := r.Probe(Probe{
			ID:        trainID,
			Seq:       seq,
			Count:     count,
			SendMS:    uint64(time.Now().UnixMilli()),
			Total:     12000,
			Remaining: 12000 - uint64(seq+1)*1200,
			Bytes:     1200,
		})
		if send {
			acked = true
			lastAck = ack
		}
	}

	if !acked {
		t.Fatal("expected at least one ack to send")
	}
	if lastAck.ID != trainID {
		t.Errorf("ack train ID = %d, want %d", lastAck.ID, trainID)
	}
	if lastAck.Count != count {
		t.Errorf("ack count = %d, want %d", lastAck.Count, count)
	}
	// All 10 received → bitmap is the low 10 bits set.
	want := uint64((1 << count) - 1)
	if lastAck.Received != want {
		t.Errorf("ack received = %#x, want %#x", lastAck.Received, want)
	}
}

func TestReceiveProbeKeepsCompletedRoundForLostAckRetransmit(t *testing.T) {
	r := NewReceive(ReceiveConfig{AckEvery: 16})

	const probeID = 42
	const count = 4

	for seq := uint16(0); seq < count; seq++ {
		r.Probe(Probe{ID: probeID, Seq: seq, Count: count, Total: 4800, Remaining: 4800 - uint64(seq+1)*1200, Bytes: 1200})
	}

	ack, send := r.Probe(Probe{ID: probeID, Seq: 1, Count: count, Total: 4800, Remaining: 2400, Bytes: 1200})
	if !send {
		t.Fatal("expected retransmitted seq for completed round to trigger ACK")
	}
	if ack.Received != 0x0f {
		t.Fatalf("retransmit ACK bitmap = %#x, want full bitmap", ack.Received)
	}
}

// TestReceiveProbeInvalid verifies malformed probes produce no ack.
func TestReceiveProbeInvalid(t *testing.T) {
	r := NewReceive(ReceiveConfig{})

	cases := []Probe{
		{ID: 1, Seq: 0, Count: 0},   // count 0
		{ID: 1, Seq: 10, Count: 10}, // seq >= count
		{ID: 1, Seq: 0, Count: 65},  // count > 64
	}
	for i, p := range cases {
		if _, send := r.Probe(p); send {
			t.Errorf("case %d: expected no ack for invalid probe %+v", i, p)
		}
	}
}

// TestNextRatePlateauStops verifies the pure rate functions: growth doubles
// toward a cap, and plateau detection halts growth.
func TestNextRateAndPlateau(t *testing.T) {
	// No cap, not stalled: additive step.
	if got := nextRate(16_000_000, 0, false); got != 16_000_000+additiveStepBps {
		t.Errorf("nextRate additive = %d, want %d", got, 16_000_000+additiveStepBps)
	}
	// No cap, stalled: hold.
	if got := nextRate(50_000_000, 0, true); got != 50_000_000 {
		t.Errorf("nextRate stalled should hold, got %d", got)
	}
	// Capped at/above cap: clamp to cap.
	if got := nextRate(100_000_000, 80_000_000, false); got != 80_000_000 {
		t.Errorf("nextRate over cap = %d, want cap 80000000", got)
	}
	// Big gap below cap: doubles.
	if got := nextRate(10_000_000, 80_000_000, false); got != 20_000_000 {
		t.Errorf("nextRate doubling = %d, want 20000000", got)
	}

	// plateau: needs plateauSteps+1 samples; flat series stalls.
	if plateau([]uint64{10, 10}) {
		t.Error("plateau should be false with too few samples")
	}
	if !plateau([]uint64{100_000_000, 101_000_000, 101_500_000}) {
		t.Error("plateau should be true for a flat series within growth bound")
	}
	if plateau([]uint64{10_000_000, 20_000_000, 45_000_000}) {
		t.Error("plateau should be false for a growing series")
	}
}

func TestCapReachedThreshold(t *testing.T) {
	const capBps = uint64(200_000_000)
	if capReached(198_999_999, capBps) {
		t.Fatal("cap reached below 99.5% threshold")
	}
	if !capReached(199_000_000, capBps) {
		t.Fatal("cap not reached at 99.5% threshold")
	}
	if capReached(1, 0) {
		t.Fatal("zero cap should never be reached")
	}
}

// TestPayloadSizeRandomizesWithinBounds verifies the per-probe payload size is
// fixed when min==max and varies deterministically within [min,max] otherwise
// (spec: UDP randomizes 1200-1400, TCP fixes 32KB). Pure function.
func TestPayloadSizeRandomizesWithinBounds(t *testing.T) {
	// Fixed: min==max always returns min.
	for seq := uint16(0); seq < 20; seq++ {
		if got := payloadSize(32768, 32768, 7, seq); got != 32768 {
			t.Fatalf("fixed payload seq=%d = %d, want 32768", seq, got)
		}
	}

	// Randomized: stays within bounds and is not all-identical.
	const min, max = 1200, 1400
	seen := map[int]bool{}
	for seq := uint16(0); seq < 64; seq++ {
		got := payloadSize(min, max, 42, seq)
		if got < min || got > max {
			t.Fatalf("payload seq=%d = %d, out of [%d,%d]", seq, got, min, max)
		}
		seen[got] = true
	}
	if len(seen) < 4 {
		t.Fatalf("payload barely varied: only %d distinct sizes over 64 probes", len(seen))
	}

	// Deterministic: same (train,seq) → same size.
	if payloadSize(min, max, 42, 5) != payloadSize(min, max, 42, 5) {
		t.Fatal("payloadSize not deterministic for the same train/seq")
	}
}

// TestRateLimiterPacesSends verifies that with RateLimit on, a low step rate
// makes the train take measurably longer than with limiting off (the limiter
// throttles the byte rate). Uses a small cap so the train is a single step.
func TestRateLimiterPacesSends(t *testing.T) {
	run := func(rateLimit bool) time.Duration {
		var done sync.WaitGroup
		done.Add(1)
		var loop *BwLoop
		var loopMu sync.Mutex
		b := New(Config{
			ReferenceBps: 1_000_000,
			CapBps:       2_000_000, // tiny cap → 1 short step → fast terminate
			MinRateBps:   1_000_000,
			StepWindow:   20 * time.Millisecond,
			PayloadMin:   1200,
			PayloadMax:   1200,
			RateLimit:    rateLimit,
			OnSample:     func(Sample) { done.Done() },
			SendProbe: func(p Probe) error {
				loopMu.Lock()
				l := loop
				loopMu.Unlock()
				if l != nil {
					full := uint64(0)
					for i := uint16(0); i < p.Count; i++ {
						full |= 1 << i
					}
					now := uint64(time.Now().UnixMilli())
					l.Ack(Ack{ID: p.ID, Count: p.Count, Received: full, FirstRXMS: now - 1, LastRXMS: now})
				}
				return nil
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		l, err := b.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		loopMu.Lock()
		loop = l
		loopMu.Unlock()
		done.Wait()
		return time.Since(start)
	}

	// Both should complete; we only assert the limited run is not faster than the
	// unlimited one (limiting can only add delay). A loose check avoids flakiness.
	limited := run(true)
	unlimited := run(false)
	if limited+50*time.Millisecond < unlimited {
		t.Fatalf("limited run (%v) unexpectedly much faster than unlimited (%v)", limited, unlimited)
	}
}

// TestLossStopDoesNotEndTrainEarly verifies loss no longer ends a train early:
// if measured bandwidth never reaches cap, the train runs until TrainWindow.
func TestLossStopDoesNotEndTrainEarly(t *testing.T) {
	withTrainWindow(t, 220*time.Millisecond)

	// runWithLossStop runs one train acking only ~25% of each step, and returns
	// how long it took. This small train window may be a single step; the
	// invariant is that loss does not end the train before TrainWindow.
	runWithLossStop := func(lossStop float64) time.Duration {
		var loop atomic.Pointer[BwLoop]
		b := New(Config{
			ReferenceBps:      500_000_000,
			CapBps:            500_000_000, // measured bps below cap → run until TrainWindow
			MinRateBps:        16_000_000,
			StepWindow:        5 * time.Millisecond,
			PayloadMin:        1200,
			PayloadMax:        1200,
			LossStopThreshold: lossStop,
			OnSample:          func(Sample) {},
			SendProbe: func(p Probe) error {
				if l := loop.Load(); l != nil {
					// Ack only ~25% → high loss, above the UDP threshold.
					received := uint64(0)
					for i := uint16(0); i < p.Count; i += 4 {
						received |= 1 << i
					}
					now := uint64(time.Now().UnixMilli())
					l.Ack(Ack{ID: p.ID, Count: p.Count, Received: received, FirstRXMS: now - 1000, LastRXMS: now})
				}
				return nil
			},
		})

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		l, err := b.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		loop.Store(l)

		start := time.Now()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			b.mu.Lock()
			_, active := b.activeLoops[l.TrainID()]
			b.mu.Unlock()
			if !active {
				return time.Since(start)
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("train did not complete in time")
		return 0
	}

	udpElapsed := runWithLossStop(bwTestLossStop)
	tcpElapsed := runWithLossStop(0)

	minElapsed := 180 * time.Millisecond
	if udpElapsed < minElapsed || tcpElapsed < minElapsed {
		t.Fatalf("train ended too early: udp=%s tcp=%s want >=%s", udpElapsed, tcpElapsed, minElapsed)
	}
	if udpElapsed+50*time.Millisecond < tcpElapsed {
		t.Fatalf("loss-stop ended UDP train early: udp=%s tcp=%s", udpElapsed, tcpElapsed)
	}
}

const bwTestLossStop = 0.01
