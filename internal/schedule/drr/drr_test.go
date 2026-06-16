package drr

import "testing"

const testMTU = 1500

type testLane struct {
	id     uint8
	weight uint32
}

func (l testLane) Weight() uint32 {
	return l.weight
}

// TestDRREqualWeightsProduceShortBursts pins the core scheduling shape: with
// baseQuantum = 4*MTU and MTU-sized cost, each equal-weight lane receives a
// short burst of ~4 consecutive picks before the next lane becomes preferred.
func TestDRREqualWeightsProduceShortBursts(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 1}, {id: 2, weight: 1}}

	var picks []uint8
	for i := 0; i < 12; i++ {
		lane, ok := s.Pick(lanes, testMTU)
		if !ok {
			t.Fatalf("pick %d returned empty", i)
		}
		picks = append(picks, lane.id)
	}

	want := []uint8{1, 1, 1, 1, 2, 2, 2, 2, 1, 1, 1, 1}
	for i := range want {
		if picks[i] != want[i] {
			t.Fatalf("pick sequence = %v, want %v", picks, want)
		}
	}
}

// TestDRRWeightsAffectLongRunSelection verifies long-run byte fairness follows
// lane weight: a weight-2 lane receives ~2x the selections of a weight-1 lane
// at equal cost.
func TestDRRWeightsAffectLongRunSelection(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 2}, {id: 2, weight: 1}}

	counts := map[uint8]int{}
	const n = 600
	for i := 0; i < n; i++ {
		lane, ok := s.Pick(lanes, testMTU)
		if !ok {
			t.Fatalf("pick %d returned empty", i)
		}
		counts[lane.id]++
	}
	ratio := float64(counts[1]) / float64(counts[2])
	if ratio < 1.8 || ratio > 2.2 {
		t.Fatalf("weight-2:weight-1 selection ratio = %.2f (counts=%v), want ~2.0", ratio, counts)
	}
}

// TestDRRVariableCostSubtractsPassedCost proves Pick subtracts the exact cost
// passed (not a fixed packet count): a quantum of 4*MTU at cost MTU/2 yields ~8
// picks per turn instead of 4.
func TestDRRVariableCostSubtractsPassedCost(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 1}, {id: 2, weight: 1}}

	const cost = testMTU / 2 // 750; 4*1500 / 750 = 8 picks per turn
	first := 0
	for i := 0; i < 8; i++ {
		lane, ok := s.Pick(lanes, cost)
		if !ok || lane.id != 1 {
			t.Fatalf("pick %d = (%+v,%v), want lane 1", i, lane, ok)
		}
		first++
	}
	if first != 8 {
		t.Fatalf("lane 1 burst = %d picks, want 8", first)
	}
	lane, ok := s.Pick(lanes, cost)
	if !ok || lane.id != 2 {
		t.Fatalf("pick after burst = (%+v,%v), want lane 2", lane, ok)
	}
}

// TestDRRZeroWeightIsIgnored verifies weight-0 lanes are never schedulable.
func TestDRRZeroWeightIsIgnored(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 0}, {id: 2, weight: 1}}

	for i := 0; i < 10; i++ {
		lane, ok := s.Pick(lanes, testMTU)
		if !ok {
			t.Fatalf("pick %d returned empty", i)
		}
		if lane.id != 2 {
			t.Fatalf("pick %d = lane %d, want lane 2 (zero-weight lane must be ignored)", i, lane.id)
		}
	}
}

// TestDRRDoesNotSpinOnEmptyOrInvalidInput verifies degenerate inputs return
// false promptly instead of spinning.
func TestDRRDoesNotSpinOnEmptyOrInvalidInput(t *testing.T) {
	s := New[testLane](4 * testMTU)

	if lane, ok := s.Pick(nil, testMTU); ok {
		t.Fatalf("empty pick = (%+v,true), want false", lane)
	}
	if lane, ok := s.Pick([]testLane{}, testMTU); ok {
		t.Fatalf("zero-length pick = (%+v,true), want false", lane)
	}
	if lane, ok := s.Pick([]testLane{{id: 1, weight: 0}, {id: 2, weight: 0}}, testMTU); ok {
		t.Fatalf("all-zero-weight pick = (%+v,true), want false", lane)
	}
}

// TestDRRLargeCostReturnsFalse verifies a cost larger than one weighted quantum
// added to the current deficit returns false (no single Pick selects it).
func TestDRRLargeCostReturnsFalse(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 1}}

	// One quantum = 4*1500 = 6000; cost 10000 cannot be covered by a single
	// quantum add against a zero deficit.
	if lane, ok := s.Pick(lanes, 10000); ok {
		t.Fatalf("large-cost pick = (%+v,true), want false", lane)
	}
}

// TestDRRDynamicLaneSetDoesNotCorrupt verifies adding/removing runnable lanes
// between Pick calls never returns a lane outside the current set and never
// panics or stalls.
func TestDRRDynamicLaneSetDoesNotCorrupt(t *testing.T) {
	s := New[testLane](4 * testMTU)
	a := testLane{id: 1, weight: 1}
	b := testLane{id: 2, weight: 1}
	c := testLane{id: 3, weight: 1}

	// Build some deficit state across a 2-lane set.
	for i := 0; i < 6; i++ {
		if _, ok := s.Pick([]testLane{a, b}, testMTU); !ok {
			t.Fatalf("warmup pick %d empty", i)
		}
	}
	// Remove b, add c; only {a,c} are valid now.
	for i := 0; i < 6; i++ {
		lane, ok := s.Pick([]testLane{a, c}, testMTU)
		if !ok {
			t.Fatalf("dynamic pick %d empty", i)
		}
		if lane.id != 1 && lane.id != 3 {
			t.Fatalf("dynamic pick %d = lane %d, want one of {1,3}", i, lane.id)
		}
	}
	// Re-add b; the retained item must not corrupt selection.
	for i := 0; i < 6; i++ {
		lane, ok := s.Pick([]testLane{a, b, c}, testMTU)
		if !ok {
			t.Fatalf("re-add pick %d empty", i)
		}
		if lane.id != 1 && lane.id != 2 && lane.id != 3 {
			t.Fatalf("re-add pick %d = lane %d, want one of {1,2,3}", i, lane.id)
		}
	}
}
