package recv

import "github.com/MeteorsLiu/multipath/internal/transport"

// frameCategory distinguishes the two FEC-bearing frame classes the receive
// accounting tracks. Control frames are not accounted.
type frameCategory uint8

const (
	catData frameCategory = iota
	catRepair
	numCategories
)

// numKinds is the size of the carrier-kind axis (UDP, TCP).
const numKinds = 2

// legArrival is one (kind, category) cell: link-delivery counters recorded
// before dedupe so they reflect what actually arrived on the wire.
type legArrival struct {
	count uint64
	bytes uint64
}

// laneArrivalStats is the per-lane QoS arrival ledger that sits beside that
// lane's rxWindow (spec 8.3/8.4 "records ... arrival information needed by
// QoS"). It is indexed [kindIndex][category] over a fixed two-kind universe, so
// it allocates nothing per packet.
//
// This is a RESERVED hook for FEC-differential QoS: its intended consumer
// compares, per lane, the carrier kind delivering DATA against the kind
// delivering REPAIR — when they are the same the lane is running single
// transport and differential detection must be skipped. This round only writes
// the ledger; nothing reads it for a decision, and it is not exported.
//
// All access happens under the owning recvState.mu, so it carries no lock.
type laneArrivalStats struct {
	cells [numKinds][numCategories]legArrival
}

// kindIndex maps a transport.Kind to its ledger row, reporting ok=false for any
// kind outside the {UDP, TCP} universe.
func kindIndex(k transport.Kind) (int, bool) {
	switch k {
	case transport.KindUDP:
		return 0, true
	case transport.KindTCP:
		return 1, true
	default:
		return 0, false
	}
}

// recordArrival bumps the (kind, category) cell by one frame of n bytes. Frames
// on an unknown carrier kind are ignored. Caller holds recvState.mu.
func (s *laneArrivalStats) recordArrival(kind transport.Kind, cat frameCategory, n int) {
	if s == nil || cat >= numCategories {
		return
	}
	idx, ok := kindIndex(kind)
	if !ok {
		return
	}
	cell := &s.cells[idx][cat]
	cell.count++
	cell.bytes += uint64(n)
}

// arrival returns the (kind, category) cell. Caller holds recvState.mu. Used by
// tests and, in a later round, by QoS detection.
func (s *laneArrivalStats) arrival(kind transport.Kind, cat frameCategory) legArrival {
	if s == nil || cat >= numCategories {
		return legArrival{}
	}
	idx, ok := kindIndex(kind)
	if !ok {
		return legArrival{}
	}
	return s.cells[idx][cat]
}
