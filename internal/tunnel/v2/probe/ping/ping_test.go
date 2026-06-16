package ping

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPingStartSendsMessages(t *testing.T) {
	var sentMessages []Message
	sendMsg := func(msg Message) error {
		sentMessages = append(sentMessages, msg)
		return nil
	}

	p := New(Config{
		Interval: 50 * time.Millisecond,
		Timeout:  1 * time.Second,
		SendMsg:  sendMsg,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_ = p.Start(ctx)

	// Should have sent at least 2 messages (one immediate, one after 50ms, possibly one after 100ms)
	if len(sentMessages) < 2 {
		t.Errorf("expected at least 2 messages, got %d", len(sentMessages))
	}

	// Verify IDs are sequential
	for i, msg := range sentMessages {
		if msg.ID != uint64(i) {
			t.Errorf("message %d: expected ID %d, got %d", i, i, msg.ID)
		}
		if msg.TimeMS == 0 {
			t.Errorf("message %d: TimeMS is zero", i)
		}
	}
}

func TestPingPongReturnsRTTQuality(t *testing.T) {
	p := New(Config{
		Interval: 1 * time.Second,
		Timeout:  5 * time.Second,
		SendMsg:  func(Message) error { return nil },
	})

	// Simulate sending a ping
	sendTime := uint64(1000)
	p.mu.Lock()
	p.pending[42] = sendTime
	p.mu.Unlock()

	// Simulate receiving a pong 50ms later
	recvTime := sendTime + 50
	pong := Message{ID: 42, TimeMS: sendTime}

	quality, ok := p.Pong(pong, recvTime)
	if !ok {
		t.Fatal("expected valid pong")
	}

	if quality.SampleMS != 50 {
		t.Errorf("expected sample 50ms, got %dms", quality.SampleMS)
	}

	if quality.SRTTMS != 50 {
		t.Errorf("expected SRTT 50ms (first sample), got %dms", quality.SRTTMS)
	}

	if quality.Samples != 1 {
		t.Errorf("expected 1 sample, got %d", quality.Samples)
	}
}

func TestPingDropsMismatchedPong(t *testing.T) {
	p := New(Config{
		Interval: 1 * time.Second,
		Timeout:  5 * time.Second,
		SendMsg:  func(Message) error { return nil },
	})

	// Simulate sending a ping
	sendTime := uint64(1000)
	p.mu.Lock()
	p.pending[42] = sendTime
	p.mu.Unlock()

	// Try pong with wrong TimeMS
	pong := Message{ID: 42, TimeMS: sendTime + 10}
	quality, ok := p.Pong(pong, sendTime+50)

	if ok {
		t.Error("expected invalid pong due to mismatched TimeMS")
	}

	if quality.Samples != 0 {
		t.Error("expected zero quality for invalid pong")
	}

	// Try pong with unknown ID
	pong = Message{ID: 999, TimeMS: sendTime}
	quality, ok = p.Pong(pong, sendTime+50)

	if ok {
		t.Error("expected invalid pong due to unknown ID")
	}
}

func TestPingSendCallbackError(t *testing.T) {
	expectedErr := errors.New("send failed")
	p := New(Config{
		Interval: 50 * time.Millisecond,
		Timeout:  1 * time.Second,
		SendMsg:  func(Message) error { return expectedErr },
	})

	ctx := context.Background()
	err := p.Start(ctx)

	if err != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestPingCleanupExpired(t *testing.T) {
	p := New(Config{
		Interval: 1 * time.Second,
		Timeout:  100 * time.Millisecond,
		SendMsg:  func(Message) error { return nil },
	})

	now := time.Now()
	oldTime := uint64(now.Add(-200 * time.Millisecond).UnixMilli())
	recentTime := uint64(now.Add(-50 * time.Millisecond).UnixMilli())

	p.mu.Lock()
	p.pending[1] = oldTime
	p.pending[2] = recentTime
	p.mu.Unlock()

	p.cleanupExpired(now)

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.pending[1]; exists {
		t.Error("expected expired ping to be cleaned up")
	}

	if _, exists := p.pending[2]; !exists {
		t.Error("expected recent ping to remain")
	}
}

// loseOnce simulates one consecutive ping timeout: inject one already-expired
// pending entry, then run cleanupExpired so it is reaped as a loss.
func loseOnce(p *Ping, id uint64) {
	now := time.Now()
	expired := uint64(now.Add(-10 * p.timeout).UnixMilli())
	p.mu.Lock()
	p.pending[id] = expired
	p.mu.Unlock()
	p.cleanupExpired(now)
}

// pongOnce simulates one successful pong (a fresh ping immediately answered).
func pongOnce(p *Ping, id uint64) (Quality, bool) {
	sendMS := uint64(time.Now().UnixMilli())
	p.mu.Lock()
	p.pending[id] = sendMS
	p.mu.Unlock()
	return p.Pong(Message{ID: id, TimeMS: sendMS}, sendMS+10)
}

// TestPingOnDownFiresOnceAtMaxLoss verifies the path is declared dead exactly
// once after MaxLoss consecutive timeouts (spec 5.5).
func TestPingOnDownFiresOnceAtMaxLoss(t *testing.T) {
	var downs int
	p := New(Config{
		Interval: time.Second,
		Timeout:  50 * time.Millisecond,
		SendMsg:  func(Message) error { return nil },
		MaxLoss:  3,
		OnDown:   func() { downs++ },
	})

	// First two losses: not yet dead.
	loseOnce(p, 1)
	loseOnce(p, 2)
	if downs != 0 {
		t.Fatalf("OnDown fired %d times before MaxLoss, want 0", downs)
	}

	// Third loss crosses MaxLoss → dead, OnDown fires once.
	loseOnce(p, 3)
	if downs != 1 {
		t.Fatalf("OnDown fired %d times at MaxLoss, want 1", downs)
	}

	// Further losses while already dead must NOT re-fire OnDown.
	loseOnce(p, 4)
	loseOnce(p, 5)
	if downs != 1 {
		t.Fatalf("OnDown re-fired while dead: %d, want 1", downs)
	}
}

// TestPingOnUpFiresAfterRecoverSuccess verifies a dead path recovers and OnUp
// fires exactly once after RecoverSuccess consecutive pongs (spec 5.5).
func TestPingOnUpFiresAfterRecoverSuccess(t *testing.T) {
	var downs, ups int
	p := New(Config{
		Interval:       time.Second,
		Timeout:        50 * time.Millisecond,
		SendMsg:        func(Message) error { return nil },
		MaxLoss:        3,
		RecoverSuccess: 3,
		OnDown:         func() { downs++ },
		OnUp:           func() { ups++ },
	})

	// Drive the path dead.
	loseOnce(p, 1)
	loseOnce(p, 2)
	loseOnce(p, 3)
	if downs != 1 {
		t.Fatalf("OnDown = %d, want 1", downs)
	}

	// First two recovery pongs: not yet alive.
	pongOnce(p, 10)
	pongOnce(p, 11)
	if ups != 0 {
		t.Fatalf("OnUp fired %d times before RecoverSuccess, want 0", ups)
	}

	// Third pong crosses RecoverSuccess → alive, OnUp fires once.
	pongOnce(p, 12)
	if ups != 1 {
		t.Fatalf("OnUp fired %d times at RecoverSuccess, want 1", ups)
	}

	// Further pongs while alive must NOT re-fire OnUp.
	pongOnce(p, 13)
	if ups != 1 {
		t.Fatalf("OnUp re-fired while alive: %d, want 1", ups)
	}
}

// TestPingPongResetsLossCount verifies a successful pong clears accumulated loss
// so a near-death path that recovers doesn't trip OnDown on the next loss (spec 5.5).
func TestPingPongResetsLossCount(t *testing.T) {
	var downs int
	p := New(Config{
		Interval: time.Second,
		Timeout:  50 * time.Millisecond,
		SendMsg:  func(Message) error { return nil },
		MaxLoss:  3,
		OnDown:   func() { downs++ },
	})

	// Two losses (one short of MaxLoss), then a pong resets the counter.
	loseOnce(p, 1)
	loseOnce(p, 2)
	pongOnce(p, 3)

	// Two more losses: counter restarted, still below MaxLoss → no OnDown.
	loseOnce(p, 4)
	loseOnce(p, 5)
	if downs != 0 {
		t.Fatalf("OnDown fired %d times, want 0 (pong should have reset lossCount)", downs)
	}

	// Third consecutive loss now crosses MaxLoss.
	loseOnce(p, 6)
	if downs != 1 {
		t.Fatalf("OnDown = %d after 3 consecutive losses, want 1", downs)
	}
}

// TestPingInitDeadActivatesOnFirstPongs verifies a ping that starts dead (the
// leg-inactive default) comes up after RecoverSuccess pongs and fires OnUp once,
// without ever firing OnDown — this is the initial-activation path (spec 5.5:
// 初次激活靠首批 PONG, not HELLO_ACK).
func TestPingInitDeadActivatesOnFirstPongs(t *testing.T) {
	var downs, ups int
	p := New(Config{
		Interval:       time.Second,
		Timeout:        50 * time.Millisecond,
		SendMsg:        func(Message) error { return nil },
		RecoverSuccess: 2,
		InitDead:       true,
		OnDown:         func() { downs++ },
		OnUp:           func() { ups++ },
	})

	pongOnce(p, 1)
	if ups != 0 {
		t.Fatalf("OnUp fired %d times before RecoverSuccess, want 0", ups)
	}

	pongOnce(p, 2)
	if ups != 1 {
		t.Fatalf("OnUp fired %d times at RecoverSuccess, want 1 (initial activation)", ups)
	}
	if downs != 0 {
		t.Fatalf("OnDown fired %d times during initial activation, want 0", downs)
	}
}
func TestPingObserverReceivesSamples(t *testing.T) {
	var samples []Quality
	p := New(Config{
		Interval: time.Second,
		Timeout:  5 * time.Second,
		SendMsg:  func(Message) error { return nil },
		Observer: func(q Quality) { samples = append(samples, q) },
	})

	pongOnce(p, 1)
	pongOnce(p, 2)

	if len(samples) != 2 {
		t.Fatalf("Observer received %d samples, want 2", len(samples))
	}
	if samples[0].Samples != 1 || samples[1].Samples != 2 {
		t.Fatalf("Observer sample counts = %d,%d, want 1,2", samples[0].Samples, samples[1].Samples)
	}
}

// TestPingOnDeliveryReportsOutcomes verifies each ping outcome is reported to
// OnDelivery: a pong → true, a timeout → false (spec 5.4 delivery-rate feed).
func TestPingOnDeliveryReportsOutcomes(t *testing.T) {
	var oks, fails int
	p := New(Config{
		Interval: time.Second,
		Timeout:  50 * time.Millisecond,
		SendMsg:  func(Message) error { return nil },
		OnDelivery: func(onTime bool) {
			if onTime {
				oks++
			} else {
				fails++
			}
		},
	})

	// Two successful pongs → two onTime=true.
	pongOnce(p, 1)
	pongOnce(p, 2)
	if oks != 2 {
		t.Fatalf("delivery oks = %d, want 2", oks)
	}

	// Three timeouts → three onTime=false.
	loseOnce(p, 10)
	loseOnce(p, 11)
	loseOnce(p, 12)
	if fails != 3 {
		t.Fatalf("delivery fails = %d, want 3", fails)
	}
}
