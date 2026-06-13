package recv

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

// frameCategory distinguishes the two FEC-bearing frame classes the receive
// accounting tracks. Control frames are not accounted.
type frameCategory uint8

const (
	catData frameCategory = iota
	catRepair
	numCategories
)

const (
	// numKinds is the size of the carrier-kind axis (UDP, TCP).
	numKinds          = 2
	qosSampleInterval = 3 * time.Second
)

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
// The ledger also owns sample cursors used to produce aggregate QoS estimator
// inputs. It does not retain packet ids, FEC groups, or a sliding packet window.
//
// All access happens under the owning recvState.mu, so it carries no lock.
type laneArrivalStats struct {
	cells          [numKinds][numCategories]legArrival
	recoveredBytes [numKinds]uint64
	lastSample     [numKinds]qosSampleCursor
}

type qosSampleCursor struct {
	at        time.Time
	data      legArrival
	sameData  legArrival
	repair    legArrival
	recovered uint64
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

func (s *laneArrivalStats) recordRecovered(kind transport.Kind, n uint64) {
	if s == nil || n == 0 {
		return
	}
	idx, ok := kindIndex(kind)
	if !ok {
		return
	}
	s.recoveredBytes[idx] += n
}

// arrival returns the (kind, category) cell. Caller holds recvState.mu. Used by
// tests and QoS sampling.
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

func (s *laneArrivalStats) qosDelta(dataKind transport.Kind, at time.Time) (qosSample, bool) {
	if s == nil || !qosKnownKind(dataKind) {
		return qosSample{}, false
	}
	idx, ok := kindIndex(dataKind)
	if !ok {
		return qosSample{}, false
	}
	repairKind := otherTransportKind(dataKind)
	data := s.arrival(dataKind, catData)
	sameData := s.arrival(repairKind, catData)
	repair := s.arrival(repairKind, catRepair)
	recovered := s.recoveredBytes[idx]
	last := s.lastSample[idx]
	if last.at.IsZero() {
		s.lastSample[idx] = qosSampleCursor{at: at, data: data, sameData: sameData, repair: repair, recovered: recovered}
		return qosSample{}, false
	}
	duration := at.Sub(last.at)
	if duration < qosSampleInterval {
		return qosSample{}, false
	}
	dataDelta := subtractArrival(data, last.data)
	sameDataDelta := subtractArrival(sameData, last.sameData)
	repairDelta := subtractArrival(repair, last.repair)
	recoveredDelta := recovered - last.recovered
	s.lastSample[idx] = qosSampleCursor{at: at, data: data, sameData: sameData, repair: repair, recovered: recovered}
	if dataDelta.count == 0 && repairDelta.count == 0 && recoveredDelta == 0 {
		return qosSample{}, false
	}
	if dataDelta.count == 0 && repairDelta.count > 0 && sameDataDelta.count > 0 && sameDataDelta.count >= repairDelta.count*maxFECSourceSpan {
		return qosSample{}, false
	}
	expected := dataDelta.count
	if repairDelta.count*maxFECSourceSpan > expected {
		expected = repairDelta.count * maxFECSourceSpan
	}
	return qosSample{
		At:             at,
		Duration:       duration,
		DataKind:       dataKind,
		DataArrived:    dataDelta.count,
		DataExpected:   expected,
		DataBytes:      dataDelta.bytes,
		RecoveredBytes: recoveredDelta,
		RepairKind:     repairKind,
		RepairBytes:    repairDelta.bytes,
	}, expected > 0
}

func subtractArrival(now, last legArrival) legArrival {
	out := legArrival{}
	if now.count > last.count {
		out.count = now.count - last.count
	}
	if now.bytes > last.bytes {
		out.bytes = now.bytes - last.bytes
	}
	return out
}
