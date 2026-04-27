package cfs

import (
	"math"
	"testing"
)

type testLane struct {
	id     uint8
	weight uint32
}

func (l testLane) Weight() uint32 {
	return l.weight
}

func TestStrategyOrdersByVirtualRuntime(t *testing.T) {
	s := New[testLane]()
	lanes := []testLane{
		{id: 1, weight: 1},
		{id: 2, weight: 1},
	}

	lane, ok := s.Pick(lanes, 100)
	if !ok || lane.id != 1 {
		t.Fatalf("first pick = (%+v,%v), want lane 1", lane, ok)
	}
	lane, ok = s.Pick(lanes, 100)
	if !ok || lane.id != 2 {
		t.Fatalf("second pick = (%+v,%v), want lane 2", lane, ok)
	}
}

func TestStrategyWeightAffectsCost(t *testing.T) {
	s := New[testLane]()
	lanes := []testLane{
		{id: 1, weight: 1},
		{id: 2, weight: 4},
	}

	counts := map[uint8]int{}
	for i := 0; i < 50; i++ {
		lane, ok := s.Pick(lanes, 100)
		if !ok {
			t.Fatalf("pick %d returned empty", i)
		}
		counts[lane.id]++
	}
	if counts[2] <= counts[1] {
		t.Fatalf("weighted lane was not favored: counts=%v", counts)
	}
}

func TestStrategySkipsInvalidWeight(t *testing.T) {
	s := New[testLane]()
	lanes := []testLane{
		{id: 1, weight: 0},
		{id: 2, weight: 1},
	}

	lane, ok := s.Pick(lanes, 100)
	if !ok || lane.id != 2 {
		t.Fatalf("pick = (%+v,%v), want lane 2", lane, ok)
	}
}

func TestStrategyClampsReturningLaneToMinimumRuntime(t *testing.T) {
	s := New[testLane]()
	returning := testLane{id: 1, weight: 1}
	active := testLane{id: 2, weight: 1}

	lane, ok := s.Pick([]testLane{returning}, 100)
	if !ok || lane.id != 1 {
		t.Fatalf("initial pick = (%+v,%v), want lane 1", lane, ok)
	}
	lane, ok = s.Pick([]testLane{active}, 100)
	if !ok || lane.id != 2 {
		t.Fatalf("active pick = (%+v,%v), want lane 2", lane, ok)
	}
	lane, ok = s.Pick([]testLane{active, returning}, 0)
	if !ok || lane.id != 2 {
		t.Fatalf("returning lane pick = (%+v,%v), want lane 2", lane, ok)
	}
}

func TestStrategyReturnsEmptyWithoutCandidates(t *testing.T) {
	s := New[testLane]()
	lane, ok := s.Pick(nil, 100)
	if ok {
		t.Fatalf("pick = (%+v,true), want empty", lane)
	}

	lane, ok = s.Pick([]testLane{{id: 1}}, 100)
	if ok {
		t.Fatalf("pick invalid weight = (%+v,true), want empty", lane)
	}
}

func TestStrategyRebasesVirtualRuntimeBeforeOverflow(t *testing.T) {
	s := New[testLane]()
	lane := testLane{id: 1, weight: 1}
	s.minVR = math.MaxUint64 - 10
	s.items[lane] = &item[testLane]{
		lane:     lane,
		vruntime: s.minVR,
	}

	picked, ok := s.Pick([]testLane{lane}, 100)
	if !ok || picked.id != 1 {
		t.Fatalf("pick after rebase = (%+v,%v), want lane 1", picked, ok)
	}
	if s.minVR != 102400 {
		t.Fatalf("minVR = %d, want 102400 after rebase and cost", s.minVR)
	}
	if s.minVR == math.MaxUint64 {
		t.Fatal("minVR saturated to MaxUint64")
	}
}
