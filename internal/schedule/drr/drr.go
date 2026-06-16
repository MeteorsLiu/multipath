// Package drr implements a deficit round-robin (DRR) schedule strategy for
// send-side DATA lane selection.
//
// DRR keeps long-term per-lane byte fairness proportional to lane weight while
// preferring short consecutive bursts on the same lane, so per-lane FEC can
// accumulate a full group before the scheduler moves on. It is implemented
// purely behind schedule.Strategy and knows nothing about protocol frames,
// transports, FEC, or lane runtime internals.
package drr

import (
	"sync"

	"github.com/MeteorsLiu/multipath/internal/schedule"
)

// Strategy is the DRR schedule strategy. The zero value is not usable; call
// New. All exported behavior is guarded by mu so concurrent Send data-path
// callers can share one strategy per session, matching the cfs strategy.
type Strategy[L schedule.Lane] struct {
	mu          sync.Mutex
	items       map[L]*item[L]
	baseQuantum uint32

	// cursor names the lane to start the next scan from. It is stored as a
	// lane value rather than a slice index so a changing runnable-lane set
	// (lanes appearing/disappearing between Pick calls) cannot silently
	// point the cursor at a different lane.
	cursor    L
	cursorSet bool
}

type item[L schedule.Lane] struct {
	deficit uint64
}

// New returns a DRR strategy whose per-lane quantum is baseQuantum bytes scaled
// by lane weight. Send constructs the default strategy with baseQuantum =
// 4 * MTU so one selected lane sends roughly one full 4-DATA FEC group before an
// equal-weight lane becomes preferred.
func New[L schedule.Lane](baseQuantum uint32) *Strategy[L] {
	return &Strategy[L]{
		items:       make(map[L]*item[L]),
		baseQuantum: baseQuantum,
	}
}

// Pick selects a runnable lane for a DATA frame of the given cost in bytes.
//
// It scans the candidate lanes (those with non-zero weight) starting at the
// cursor. For each candidate it adds one weighted quantum to the deficit when
// the deficit cannot yet cover cost, then selects the first candidate whose
// deficit covers cost, subtracting cost from that lane's deficit. The cursor
// stays on the selected lane while its remaining deficit still covers cost and
// otherwise advances to the next candidate, which produces the desired
// short-burst-then-rotate behavior. If no candidate can cover cost after a
// single weighted quantum, Pick returns false without spinning.
func (s *Strategy[L]) Pick(lanes []L, cost uint32) (L, bool) {
	var zero L
	n := len(lanes)
	if n == 0 {
		return zero, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[L]*item[L])
	}

	start := 0
	if s.cursorSet {
		for i := 0; i < n; i++ {
			if lanes[i] == s.cursor {
				start = i
				break
			}
		}
	}

	costU := uint64(cost)
	for k := 0; k < n; k++ {
		idx := (start + k) % n
		lane := lanes[idx]
		weight := lane.Weight()
		if weight == 0 {
			continue
		}
		it := s.item(lane)
		if it.deficit < costU {
			it.deficit += uint64(s.baseQuantum) * uint64(weight)
		}
		if it.deficit < costU {
			// Even one weighted quantum cannot cover this cost; try the next
			// candidate rather than spinning on this one.
			continue
		}
		it.deficit -= costU
		if it.deficit >= costU {
			s.cursor, s.cursorSet = lane, true
		} else {
			s.advanceCursor(lanes, idx)
		}
		return lane, true
	}
	return zero, false
}

// advanceCursor moves the cursor to the next candidate lane after idx (with
// non-zero weight), wrapping around. When the selected lane is the only
// candidate the cursor stays on it.
func (s *Strategy[L]) advanceCursor(lanes []L, idx int) {
	n := len(lanes)
	for k := 1; k <= n; k++ {
		next := lanes[(idx+k)%n]
		if next.Weight() != 0 {
			s.cursor, s.cursorSet = next, true
			return
		}
	}
}

func (s *Strategy[L]) item(lane L) *item[L] {
	it := s.items[lane]
	if it == nil {
		it = &item[L]{}
		s.items[lane] = it
	}
	return it
}
