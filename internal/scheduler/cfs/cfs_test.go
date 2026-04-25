package cfs

import (
	"errors"
	"math"
	"testing"
)

func TestSchedulerOrdersByVirtualRuntime(t *testing.T) {
	s := New()
	if err := s.Enqueue(1, 1, 0); err != nil {
		t.Fatalf("enqueue lane 1: %v", err)
	}
	if err := s.Enqueue(2, 1, 0); err != nil {
		t.Fatalf("enqueue lane 2: %v", err)
	}

	laneID, ok := s.Dequeue()
	if !ok || laneID != 1 {
		t.Fatalf("first dequeue = (%d,%v), want (1,true)", laneID, ok)
	}
	if err := s.Enqueue(laneID, 1, 100); err != nil {
		t.Fatalf("requeue lane 1: %v", err)
	}

	laneID, ok = s.Dequeue()
	if !ok || laneID != 2 {
		t.Fatalf("second dequeue = (%d,%v), want (2,true)", laneID, ok)
	}
}

func TestSchedulerWeightAffectsCharge(t *testing.T) {
	s := New()
	if err := s.Enqueue(1, 1, 0); err != nil {
		t.Fatalf("enqueue lane 1: %v", err)
	}
	if err := s.Enqueue(2, 4, 0); err != nil {
		t.Fatalf("enqueue lane 2: %v", err)
	}

	counts := map[uint8]int{}
	for i := 0; i < 50; i++ {
		laneID, ok := s.Dequeue()
		if !ok {
			t.Fatalf("dequeue %d returned empty", i)
		}
		counts[laneID]++
		if err := s.Enqueue(laneID, map[uint8]uint32{1: 1, 2: 4}[laneID], 100); err != nil {
			t.Fatalf("requeue lane %d: %v", laneID, err)
		}
	}
	if counts[2] <= counts[1] {
		t.Fatalf("weighted lane was not favored: counts=%v", counts)
	}
}

func TestSchedulerRejectsInvalidOrDuplicateEnqueue(t *testing.T) {
	s := New()
	if err := s.Enqueue(1, 0, 0); !errors.Is(err, ErrInvalidWeight) {
		t.Fatalf("Enqueue invalid weight err = %v, want ErrInvalidWeight", err)
	}
	if err := s.Enqueue(1, 1, 0); err != nil {
		t.Fatalf("enqueue lane 1: %v", err)
	}
	if err := s.Enqueue(1, 1, 0); !errors.Is(err, ErrQueuedLane) {
		t.Fatalf("duplicate enqueue err = %v, want ErrQueuedLane", err)
	}
}

func TestSchedulerReenqueueClampsToMinVirtualRuntime(t *testing.T) {
	s := New()
	laneID, ok := s.Dequeue()
	if ok {
		t.Fatalf("initial dequeue = (%d,true), want empty", laneID)
	}

	if err := s.Enqueue(9, 1, 0); err != nil {
		t.Fatalf("enqueue inactive lane: %v", err)
	}
	laneID, ok = s.Dequeue()
	if !ok || laneID != 9 {
		t.Fatalf("dequeue inactive lane = (%d,%v), want (9,true)", laneID, ok)
	}

	if err := s.Enqueue(1, 1, 100); err != nil {
		t.Fatalf("enqueue active lane with charge: %v", err)
	}
	laneID, ok = s.Dequeue()
	if !ok || laneID != 1 {
		t.Fatalf("dequeue active lane = (%d,%v), want (1,true)", laneID, ok)
	}

	if err := s.Enqueue(1, 1, 0); err != nil {
		t.Fatalf("requeue active lane: %v", err)
	}
	if err := s.Enqueue(9, 1, 0); err != nil {
		t.Fatalf("requeue returning lane: %v", err)
	}

	laneID, ok = s.Dequeue()
	if !ok || laneID != 1 {
		t.Fatalf("dequeue after returning lane = (%d,%v), want (1,true)", laneID, ok)
	}
}

func TestSchedulerRebasesVirtualRuntimeBeforeOverflow(t *testing.T) {
	s := New()
	s.minVR = math.MaxUint64 - 10
	s.items[1] = &item{
		laneID:   1,
		weight:   1,
		vruntime: s.minVR,
		index:    -1,
	}

	if err := s.Enqueue(1, 1, 100); err != nil {
		t.Fatalf("enqueue near overflow: %v", err)
	}
	if s.minVR != 0 {
		t.Fatalf("minVR = %d, want 0 after rebase", s.minVR)
	}

	laneID, ok := s.Dequeue()
	if !ok || laneID != 1 {
		t.Fatalf("dequeue after rebase = (%d,%v), want (1,true)", laneID, ok)
	}
	if s.minVR == math.MaxUint64 {
		t.Fatal("minVR saturated to MaxUint64")
	}
}
