package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerLifecycle(t *testing.T) {
	var manager Manager

	if _, ok := manager.Get(99); ok {
		t.Fatal("Get before Create ok = true, want false")
	}

	created, ok := manager.Create(99)
	if !ok || created == nil {
		t.Fatalf("Create = (%v,%v), want session,true", created, ok)
	}
	if duplicate, ok := manager.Create(99); ok || duplicate != nil {
		t.Fatalf("duplicate Create = (%v,%v), want nil,false", duplicate, ok)
	}
	if got, ok := manager.GetOrCreate(99); !ok || got != created {
		t.Fatalf("GetOrCreate existing = (%v,%v), want original,true", got, ok)
	}

	manager.Delete(99)
	if _, ok := manager.Get(99); ok {
		t.Fatal("Get after Delete ok = true, want false")
	}
}

func TestManagerGetOrDeleteIsAtomic(t *testing.T) {
	var manager Manager
	created, ok := manager.Create(99)
	if !ok || created == nil {
		t.Fatalf("Create = (%v,%v), want session,true", created, ok)
	}

	const workers = 16
	start := make(chan struct{})
	results := make(chan *Session, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			s, ok := manager.GetOrDelete(99)
			if !ok {
				results <- nil
				return
			}
			results <- s
		}()
	}
	close(start)

	var got []*Session
	for i := 0; i < workers; i++ {
		if s := <-results; s != nil {
			got = append(got, s)
		}
	}
	if len(got) != 1 || got[0] != created {
		t.Fatalf("GetOrDelete winners = %d (%v), want exactly original session", len(got), got)
	}
	if _, ok := manager.Get(99); ok {
		t.Fatal("Get after GetOrDelete ok = true, want false")
	}
}

func TestNewGeneratesSessionID(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if s == nil {
		t.Fatal("New returned nil session")
	}
	if err := s.Do(func(v View) error {
		if v.SessionID() == 0 {
			t.Fatal("SessionID = 0, want generated non-zero id")
		}
		return nil
	}); err != nil {
		t.Fatalf("Session.Do: %v", err)
	}
}

func TestManagerAdd(t *testing.T) {
	var manager Manager
	s, err := New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	var id uint64
	if err := s.Do(func(v View) error {
		id = v.SessionID()
		return nil
	}); err != nil {
		t.Fatalf("Session.Do: %v", err)
	}
	if !manager.Add(s) {
		t.Fatal("Add = false, want true")
	}
	if got, ok := manager.Get(id); !ok || got != s {
		t.Fatalf("Get after Add = (%v,%v), want original,true", got, ok)
	}
	if manager.Add(s) {
		t.Fatal("duplicate Add = true, want false")
	}
}

// TestHelloRetriesUntilAck verifies the self-driving Hello loop sends an initial
// HELLO immediately, keeps re-sending on each RetryInterval, and stops the moment
// Ack is called (spec 5.1: Hello 自治, 立即发 → 每 Interval 重发 → Ack 停).
func TestHelloRetriesUntilAck(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(99)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	var sends atomic.Int32
	var gotNonce atomic.Uint64
	var gotSession atomic.Uint64
	sender := func(ctx context.Context, v View) error {
		gotSession.Store(v.SessionID())
		gotNonce.Store(v.Nonce() + 1) // +1 so we can tell "set" from zero default
		sends.Add(1)
		return nil
	}

	s.Open(context.Background(), HelloConfig{RetryInterval: 10 * time.Millisecond}, sender, nil)

	// Initial send is immediate.
	waitFor(t, 200*time.Millisecond, func() bool { return sends.Load() >= 1 })
	if got := gotSession.Load(); got != 99 {
		t.Fatalf("sender session = %d, want 99", got)
	}
	if got := gotNonce.Load(); got != 1 { // nonce 0 + 1
		t.Fatalf("sender nonce = %d, want 0", got-1)
	}

	// Keeps re-sending on each interval until Ack.
	waitFor(t, 500*time.Millisecond, func() bool { return sends.Load() >= 3 })

	if !s.Ack(0, true) {
		t.Fatal("Ack accepted = false, want true")
	}

	// After Ack the loop stops: capture the count, wait, expect no further sends.
	time.Sleep(20 * time.Millisecond) // let any in-flight tick settle
	stopped := sends.Load()
	time.Sleep(60 * time.Millisecond)
	if after := sends.Load(); after != stopped {
		t.Fatalf("sends continued after Ack: %d → %d", stopped, after)
	}

	// Duplicate / stale Ack is a no-op.
	if s.Ack(0, true) {
		t.Fatal("duplicate Ack = true, want false")
	}
}

// TestHelloExpiresOnTimeout verifies onExpire fires exactly once when the Hello
// times out without an Ack (spec 5.1: Timeout→onExpire).
func TestHelloExpiresOnTimeout(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(7)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	var expires atomic.Int32
	onExpire := func() { expires.Add(1) }

	sender := func(ctx context.Context, v View) error { return nil }

	s.Open(context.Background(),
		HelloConfig{RetryInterval: 10 * time.Millisecond, TimeoutMS: 30},
		sender, onExpire)

	waitFor(t, 500*time.Millisecond, func() bool { return expires.Load() >= 1 })

	// onExpire must fire exactly once.
	time.Sleep(80 * time.Millisecond)
	if got := expires.Load(); got != 1 {
		t.Fatalf("onExpire fired %d times, want 1", got)
	}
}

// TestHelloAckStopsBeforeExpire verifies Ack before the timeout prevents onExpire.
func TestHelloAckStopsBeforeExpire(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(11)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	var expires atomic.Int32
	onExpire := func() { expires.Add(1) }
	sender := func(ctx context.Context, v View) error { return nil }

	s.Open(context.Background(),
		HelloConfig{RetryInterval: 10 * time.Millisecond, TimeoutMS: 200},
		sender, onExpire)

	time.Sleep(20 * time.Millisecond)
	if !s.Ack(0, true) {
		t.Fatal("Ack accepted = false, want true")
	}

	time.Sleep(250 * time.Millisecond) // past the original timeout
	if got := expires.Load(); got != 0 {
		t.Fatalf("onExpire fired %d times after Ack, want 0", got)
	}
}

func TestHelloAckCallbackFiresOnlyWhenAccepted(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(13)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	var callbacks atomic.Int32
	sender := func(ctx context.Context, v View) error { return nil }

	s.Open(context.Background(),
		HelloConfig{RetryInterval: time.Hour, OnAck: func() { callbacks.Add(1) }},
		sender, nil)

	if !s.Ack(0, true) {
		t.Fatal("accepted Ack returned false, want true")
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("onAck callbacks after accepted Ack = %d, want 1", got)
	}

	s.Open(context.Background(),
		HelloConfig{RetryInterval: time.Hour, OnAck: func() { callbacks.Add(1) }},
		sender, nil)

	if s.Ack(1, false) {
		t.Fatal("rejected Ack returned true, want false")
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("onAck callbacks after rejected Ack = %d, want still 1", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func TestSessionDoViewHasNoNonce(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(99)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	if err := s.Do(func(v View) error {
		if v.SessionID() != 99 || v.Nonce() != 0 {
			t.Fatalf("session view = session %d nonce %d, want 99/0", v.SessionID(), v.Nonce())
		}
		return nil
	}); err != nil {
		t.Fatalf("Session.Do: %v", err)
	}
}
