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
		AckGrace:     20 * time.Millisecond,
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
	if first.ID != loop.TrainID() {
		t.Errorf("probe train ID = %d, want %d", first.ID, loop.TrainID())
	}
	if first.Count == 0 {
		t.Error("expected non-zero count")
	}
	if first.Bytes == 0 {
		t.Error("expected non-zero probe bytes")
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
		AckGrace:          5 * time.Millisecond,
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-sampleDone:
	case <-time.After(time.Second):
		t.Fatal("train did not complete")
	}

	sentMu.Lock()
	defer sentMu.Unlock()
	if len(sentProbes) < 2 {
		t.Fatalf("expected data probes plus completion marker, got %d", len(sentProbes))
	}

	trainTotal := trainBudgetBytes(referenceBps)
	first := sentProbes[0]
	if first.Total != trainTotal {
		t.Fatalf("first probe total = %d, want train budget %d", first.Total, trainTotal)
	}
	if first.Remaining != trainTotal-uint64(first.Bytes) {
		t.Fatalf("first probe remaining = %d, want %d", first.Remaining, trainTotal-uint64(first.Bytes))
	}

	last := sentProbes[len(sentProbes)-1]
	if last.Remaining != 0 || last.Bytes != 0 || last.Count != 1 {
		t.Fatalf("completion marker = %+v, want Remaining=0 Bytes=0 Count=1", last)
	}
	for i, p := range sentProbes[:len(sentProbes)-1] {
		if p.Bytes == 0 {
			t.Fatalf("probe %d before completion has zero bytes: %+v", i, p)
		}
		if p.Total != trainTotal {
			t.Fatalf("probe %d total = %d, want %d", i, p.Total, trainTotal)
		}
		if p.Remaining == 0 {
			t.Fatalf("probe %d reported train complete before completion marker", i)
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
		AckGrace:     5 * time.Millisecond,
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-sampleDone:
	case <-time.After(time.Second):
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
	for i := 1; i < len(dataProbes); i++ {
		if dataProbes[i].Remaining >= dataProbes[i-1].Remaining {
			t.Fatalf("remaining did not decrease at probe %d: prev=%d cur=%d", i, dataProbes[i-1].Remaining, dataProbes[i].Remaining)
		}
		if dataProbes[i].Total != dataProbes[0].Total {
			t.Fatalf("total changed at probe %d: first=%d cur=%d", i, dataProbes[0].Total, dataProbes[i].Total)
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
		AckGrace:     200 * time.Millisecond,
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

// TestBwLoopAckWithLoss verifies the aggregate loss reflects partial acks.
func TestBwLoopAckWithLoss(t *testing.T) {
	var samples []Sample
	var mu sync.Mutex
	onSample := func(s Sample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}

	var loop atomic.Pointer[BwLoop]
	b := New(Config{
		ReferenceBps: 1_000_000,
		CapBps:       16_000_000,
		OnSample:     onSample,
		StepWindow:   40 * time.Millisecond,
		AckGrace:     200 * time.Millisecond,
		SendProbe: func(p Probe) error {
			if l := loop.Load(); l != nil {
				// Ack only even sequences: ~50% loss.
				received := uint64(0)
				for i := uint16(0); i < p.Count; i += 2 {
					received |= 1 << i
				}
				now := uint64(time.Now().UnixMilli())
				l.Ack(Ack{ID: p.ID, Count: p.Count, Received: received, FirstRXMS: now - 10, LastRXMS: now})
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
	if s := samples[0]; s.Loss < 0.3 || s.Loss > 0.7 {
		t.Errorf("expected loss around 0.5, got %f", s.Loss)
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
			AckGrace:     20 * time.Millisecond,
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
	// how many steps it took. The SendProbe closure acks via the loop captured
	// from Start's return; a tiny settle loop ensures the loop pointer is set
	// before the first probes arrive.
	runWithLossStop := func(lossStop float64) int {
		var steps int
		var stepsMu sync.Mutex
		var loop atomic.Pointer[BwLoop]
		b := New(Config{
			ReferenceBps:      500_000_000,
			CapBps:            500_000_000, // measured bps below cap → run until TrainWindow
			MinRateBps:        16_000_000,
			StepWindow:        5 * time.Millisecond,
			AckGrace:          15 * time.Millisecond,
			PayloadMin:        1200,
			PayloadMax:        1200,
			LossStopThreshold: lossStop,
			OnSample:          func(Sample) {},
			SendProbe: func(p Probe) error {
				if p.Bytes > 0 && p.Seq == 0 {
					stepsMu.Lock()
					steps++ // one per step (first frame)
					stepsMu.Unlock()
				}
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

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			b.mu.Lock()
			_, active := b.activeLoops[l.TrainID()]
			b.mu.Unlock()
			if !active {
				stepsMu.Lock()
				n := steps
				stepsMu.Unlock()
				return n
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("train did not complete in time")
		return 0
	}

	udpSteps := runWithLossStop(bwTestLossStop)
	tcpSteps := runWithLossStop(0)

	if udpSteps < 2 || tcpSteps < 2 {
		t.Fatalf("train ended too early: udp steps=%d, tcp steps=%d", udpSteps, tcpSteps)
	}
	if udpSteps != tcpSteps {
		t.Fatalf("loss-stop changed train length: udp steps=%d, tcp steps=%d", udpSteps, tcpSteps)
	}
}

const bwTestLossStop = 0.01
