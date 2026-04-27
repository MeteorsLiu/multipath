package session

import "testing"

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

func TestSessionOpenAckAndRetry(t *testing.T) {
	var manager Manager
	s, ok := manager.GetOrCreate(99)
	if !ok {
		t.Fatal("GetOrCreate failed")
	}

	hello := s.Open(1000)
	var opened View
	if err := hello.Do(func(v View) error {
		opened = v
		return nil
	}); err != nil {
		t.Fatalf("Hello.Do: %v", err)
	}
	if opened.SessionID() != 99 || opened.Nonce() != 0 {
		t.Fatalf("hello view = session %d nonce %d, want 99/0", opened.SessionID(), opened.Nonce())
	}

	sent, expired, err := hello.Retry(2000, func(v View) error {
		if v.SessionID() != 99 || v.Nonce() != 0 {
			t.Fatalf("retry view = session %d nonce %d, want 99/0", v.SessionID(), v.Nonce())
		}
		return nil
	})
	if err != nil || !sent || expired {
		t.Fatalf("Retry = (%v,%v,%v), want true,false,nil", sent, expired, err)
	}

	if !s.Ack(0, true) {
		t.Fatal("Ack accepted = false, want true")
	}
	sent, expired, err = hello.Retry(3000, func(View) error {
		t.Fatal("Retry callback called after Ack")
		return nil
	})
	if err != nil || sent || !expired {
		t.Fatalf("Retry after Ack = (%v,%v,%v), want false,true,nil", sent, expired, err)
	}
	if s.Ack(0, true) {
		t.Fatal("duplicate Ack = true, want false")
	}
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
