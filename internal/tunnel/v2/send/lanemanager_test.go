package send

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
)

func TestLaneManagerLookupBwLoopFallsBackToSingleActiveLoop(t *testing.T) {
	m := NewLaneManager()
	loop, err := bw.New(bw.Config{SendProbe: func(bw.Probe) error { return nil }}).Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer loop.Stop()

	m.PutBwLoop(loop.TrainID(), loop)

	if got := m.LookupBwLoop(123456789); got != loop {
		t.Fatal("single active BW loop was not used for ACK-only probe routing")
	}
}

func TestLaneManagerPutBwLoopReplacesStaleLoop(t *testing.T) {
	m := NewLaneManager()
	newLoop := func() *bw.BwLoop {
		loop, err := bw.New(bw.Config{SendProbe: func(bw.Probe) error { return nil }}).Start(t.Context())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(loop.Stop)
		return loop
	}
	first := newLoop()
	second := newLoop()
	m.PutBwLoop(1, first)
	m.PutBwLoop(2, second)

	if got := m.LookupBwLoop(999); got != second {
		t.Fatalf("current loop = %+v, want second", got)
	}
	if got := m.LookupBwLoop(1); got != second {
		t.Fatalf("stale train id returned old loop: got %+v, want second", got)
	}
}
