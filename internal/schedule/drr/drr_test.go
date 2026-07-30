package drr

import "testing"

const testMTU = 1500

type testLane struct {
	id             uint8
	weight         uint32
	costMultiplier uint32
}

func (l testLane) Weight() uint32 {
	return l.weight
}

func (l testLane) Cost(cost uint32) uint32 {
	if l.costMultiplier == 0 {
		return cost
	}
	return cost * l.costMultiplier
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

func TestDRRLaneCostAffectsSelection(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{
		{id: 1, weight: 1, costMultiplier: 1},
		{id: 2, weight: 1, costMultiplier: 2},
		{id: 3, weight: 1, costMultiplier: 3},
		{id: 4, weight: 1, costMultiplier: 4},
	}

	counts := map[uint8]int{}
	for i := 0; i < 2500; i++ {
		lane, ok := s.Pick(lanes, testMTU)
		if !ok {
			t.Fatalf("pick %d returned empty", i)
		}
		counts[lane.id]++
	}
	want := map[uint8]int{1: 1200, 2: 600, 3: 400, 4: 300}
	for laneID, wantCount := range want {
		if counts[laneID] != wantCount {
			t.Fatalf("selection counts = %v, want %v", counts, want)
		}
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

// TestDRRLargeCostAccumulatesQuantum verifies temporary deficit shortage never
// escapes Pick as a false no-lane result.
func TestDRRLargeCostAccumulatesQuantum(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lanes := []testLane{{id: 1, weight: 1}}

	lane, ok := s.Pick(lanes, 10000)
	if !ok || lane.id != 1 {
		t.Fatalf("large-cost pick = (%+v,%v), want lane 1", lane, ok)
	}
	if deficit := s.items[lanes[0]].deficit; deficit != 2000 {
		t.Fatalf("deficit after large-cost pick = %d, want 2000", deficit)
	}
}

func TestDRRRepairCostAboveQuantumStillSelects(t *testing.T) {
	s := New[testLane](4 * testMTU)
	lane := testLane{id: 1, weight: 1, costMultiplier: 4}

	picked, ok := s.Pick([]testLane{lane}, 1514)
	if !ok || picked.id != lane.id {
		t.Fatalf("repair-cost pick = (%+v,%v), want lane 1", picked, ok)
	}
	if deficit := s.items[lane].deficit; deficit != 5944 {
		t.Fatalf("deficit after repair-cost pick = %d, want 5944", deficit)
	}
}

func TestDRRVirtualRoundLimitStillSelects(t *testing.T) {
	s := New[testLane](0)
	lane := testLane{id: 1, weight: 1}

	picked, ok := s.Pick([]testLane{lane}, testMTU)
	if !ok || picked.id != lane.id {
		t.Fatalf("bounded pick = (%+v,%v), want lane 1", picked, ok)
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
