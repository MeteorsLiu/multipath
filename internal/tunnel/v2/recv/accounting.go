package recv

import (
	"sort"
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
	numKinds            = 2
	qosSampleInterval   = 3 * time.Second
	qosOpenGroupLag     = 2 * time.Second
	qosAccountingRetain = 30 * time.Second
)

// legArrival is one (kind, category) cell: link-delivery counters recorded
// before DATA dedupe so they reflect what actually arrived on the wire.
type legArrival struct {
	count uint64
	bytes uint64
}

type qosDataArrival struct {
	kind transport.Kind
	at   time.Time
}

type qosDataPoint struct {
	packetID uint32
	at       time.Time
}

type qosRepairGroup struct {
	base       uint32
	span       uint8
	repairKind transport.Kind
	repairAt   time.Time
}

// laneArrivalStats is the private per-lane FEC differential ledger beside the
// lane's rxWindow. It stores packet-id waterlines, repair group facts, arrival
// bytes, and enough timing to aggregate qosSample values. The estimator never
// sees packet ids or groups.
//
// All access happens under the owning recvState.mu, so it carries no lock.
type laneArrivalStats struct {
	cells [numKinds][numCategories]legArrival

	repairWireSpan [numKinds]uint64
	recoveredBytes [numKinds]uint64

	data   map[uint32]qosDataArrival
	groups map[uint32]*qosRepairGroup

	lastSample qosSampleCursor
}

type qosSampleCursor struct {
	at             time.Time
	cells          [numKinds][numCategories]legArrival
	repairWireSpan [numKinds]uint64
	recoveredBytes [numKinds]uint64
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

func kindFromIndex(idx int) transport.Kind {
	if idx == 0 {
		return transport.KindUDP
	}
	return transport.KindTCP
}

func (s *laneArrivalStats) recordData(kind transport.Kind, packetID uint32, n int, at time.Time) {
	if s == nil {
		return
	}
	if _, ok := kindIndex(kind); !ok {
		return
	}
	s.recordArrival(kind, catData, n)
	if s.data == nil {
		s.data = make(map[uint32]qosDataArrival)
	}
	if _, exists := s.data[packetID]; !exists {
		s.data[packetID] = qosDataArrival{kind: kind, at: at}
	}
}

func (s *laneArrivalStats) recordRepair(kind transport.Kind, basePacketID uint32, sourceSpan uint8, n int, at time.Time) {
	if s == nil || sourceSpan == 0 || sourceSpan > maxFECSourceSpan {
		return
	}
	idx, ok := kindIndex(kind)
	if !ok {
		return
	}
	s.recordArrival(kind, catRepair, n)
	s.repairWireSpan[idx] += uint64(sourceSpan)
	if s.groups == nil {
		s.groups = make(map[uint32]*qosRepairGroup)
	}
	group := s.groups[basePacketID]
	if group == nil {
		s.groups[basePacketID] = &qosRepairGroup{
			base:       basePacketID,
			span:       sourceSpan,
			repairKind: kind,
			repairAt:   at,
		}
		return
	}
	if group.repairAt.IsZero() || at.Before(group.repairAt) {
		group.repairAt = at
	}
	if sourceSpan > group.span {
		group.span = sourceSpan
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

func (s *laneArrivalStats) recordRecoveredGroup(basePacketID uint32, n uint64) bool {
	if s == nil || n == 0 {
		return false
	}
	group := s.groups[basePacketID]
	if group == nil {
		return false
	}
	dataKind := otherTransportKind(group.repairKind)
	s.recordRecovered(dataKind, n)
	return true
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

func (s *laneArrivalStats) qosSamples(at time.Time) []qosSample {
	if s == nil {
		return nil
	}
	now := s.snapshot(at)
	if s.lastSample.at.IsZero() {
		s.lastSample = now
		return nil
	}
	duration := at.Sub(s.lastSample.at)
	if duration < qosSampleInterval {
		return nil
	}

	last := s.lastSample
	samples := make([]qosSample, 0, numKinds)
	for idx := 0; idx < numKinds; idx++ {
		if sample, ok := s.qosSampleForKind(idx, last, now, duration); ok {
			samples = append(samples, sample)
		}
	}
	s.lastSample = now
	s.prune(at)
	return samples
}

func (s *laneArrivalStats) snapshot(at time.Time) qosSampleCursor {
	return qosSampleCursor{
		at:             at,
		cells:          s.cells,
		repairWireSpan: s.repairWireSpan,
		recoveredBytes: s.recoveredBytes,
	}
}

func (s *laneArrivalStats) qosSampleForKind(idx int, last, now qosSampleCursor, duration time.Duration) (qosSample, bool) {
	kind := kindFromIndex(idx)
	otherIdx := 1 - idx
	otherKind := kindFromIndex(otherIdx)

	dataDelta := subtractArrival(now.cells[idx][catData], last.cells[idx][catData])
	repairDelta := subtractArrival(now.cells[idx][catRepair], last.cells[idx][catRepair])
	otherRepairDelta := subtractArrival(now.cells[otherIdx][catRepair], last.cells[otherIdx][catRepair])
	recoveredDelta := deltaU64(now.recoveredBytes[idx], last.recoveredBytes[idx])
	repairWireSpanDelta := deltaU64(now.repairWireSpan[idx], last.repairWireSpan[idx])

	dataExpected := dataDelta.count + s.missingDataSpanFromRepair(kind, otherKind, now.at)
	repairExpected := maxU64(repairWireSpanDelta, s.expectedRepairSpan(kind, last.at, now.at))

	dataSample := qosSample{
		At:             now.at,
		Duration:       duration,
		DataKind:       kind,
		DataArrived:    dataDelta.count,
		DataExpected:   dataExpected,
		DataBytes:      dataDelta.bytes,
		RecoveredBytes: recoveredDelta,
		RepairKind:     otherKind,
		RepairBytes:    otherRepairDelta.bytes,
		Lag:            s.lagMedian(kind, otherKind, last.at, now.at),
	}
	repairSample := qosSample{
		At:           now.at,
		Duration:     duration,
		DataKind:     kind,
		DataArrived:  repairWireSpanDelta,
		DataExpected: repairExpected,
		DataBytes:    repairDelta.bytes,
		RepairKind:   otherKind,
	}

	dataOK := dataSample.DataExpected > 0 || dataSample.DataArrived > 0 || dataSample.RecoveredBytes > 0
	repairOK := repairSample.DataExpected > 0 || repairSample.DataArrived > 0
	switch {
	case dataOK && repairOK:
		if dataSample.Lag > 0 {
			return dataSample, true
		}
		if lossRatio(repairSample.DataExpected, repairSample.DataArrived) > lossRatio(dataSample.DataExpected, dataSample.DataArrived) {
			return repairSample, true
		}
		return dataSample, true
	case dataOK:
		return dataSample, true
	case repairOK:
		return repairSample, true
	default:
		return qosSample{}, false
	}
}

func (s *laneArrivalStats) lagMedian(dataKind, repairKind transport.Kind, since, at time.Time) time.Duration {
	if s == nil || len(s.groups) == 0 {
		return 0
	}
	lags := make([]time.Duration, 0, 8)
	for _, group := range s.groups {
		if group == nil || group.repairKind != repairKind || group.repairAt.IsZero() {
			continue
		}
		lastDataAt, complete := s.groupLastDataAt(group, dataKind)
		eventAt := lastDataAt
		lag := lastDataAt.Sub(group.repairAt)
		if !complete {
			if at.Sub(group.repairAt) < qosOpenGroupLag {
				continue
			}
			eventAt = at
			lag = at.Sub(group.repairAt)
		}
		if !eventAt.After(since) || eventAt.After(at) {
			continue
		}
		lags = append(lags, lag)
	}
	if len(lags) == 0 {
		return 0
	}
	sort.Slice(lags, func(i, j int) bool {
		return lags[i] < lags[j]
	})
	return lags[len(lags)/2]
}

func (s *laneArrivalStats) groupLastDataAt(group *qosRepairGroup, dataKind transport.Kind) (time.Time, bool) {
	if group == nil || group.span == 0 {
		return time.Time{}, false
	}
	var last time.Time
	for offset := uint8(0); offset < group.span; offset++ {
		arrival, ok := s.data[group.base+uint32(offset)]
		if !ok || arrival.kind != dataKind {
			return time.Time{}, false
		}
		if arrival.at.After(last) {
			last = arrival.at
		}
	}
	return last, true
}

func (s *laneArrivalStats) expectedRepairSpan(repairKind transport.Kind, since, at time.Time) uint64 {
	if s == nil || len(s.data) == 0 {
		return 0
	}
	dataKind := otherTransportKind(repairKind)
	points := make([]qosDataPoint, 0, len(s.data))
	for packetID, arrival := range s.data {
		if arrival.kind != dataKind || !arrival.at.After(since) || arrival.at.After(at) {
			continue
		}
		points = append(points, qosDataPoint{packetID: packetID, at: arrival.at})
	}
	if len(points) == 0 {
		return 0
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].packetID < points[j].packetID
	})
	count := uint64(0)
	runStart := points[0].packetID
	runLen := uint8(1)
	for i := 1; i < len(points); i++ {
		if points[i].packetID == points[i-1].packetID+1 && runLen < maxFECSourceSpan {
			runLen++
			continue
		}
		count += uint64(s.expectedRepairRunSpan(runStart, runLen, dataKind, at))
		runStart = points[i].packetID
		runLen = 1
	}
	count += uint64(s.expectedRepairRunSpan(runStart, runLen, dataKind, at))
	return count
}

func (s *laneArrivalStats) missingDataSpanFromRepair(dataKind, repairKind transport.Kind, at time.Time) uint64 {
	if s == nil || len(s.groups) == 0 {
		return 0
	}
	var missing uint64
	for _, group := range s.groups {
		if group == nil || group.repairKind != repairKind || group.repairAt.IsZero() {
			continue
		}
		if group.repairAt.After(at) {
			continue
		}
		missing += uint64(s.missingGroupDataSpan(group, dataKind, at))
	}
	return missing
}

func (s *laneArrivalStats) missingGroupDataSpan(group *qosRepairGroup, dataKind transport.Kind, at time.Time) uint8 {
	if group == nil || group.span == 0 {
		return 0
	}
	var missing uint8
	for offset := uint8(0); offset < group.span; offset++ {
		arrival, ok := s.data[group.base+uint32(offset)]
		if !ok || arrival.kind != dataKind || arrival.at.After(at) {
			missing++
		}
	}
	return missing
}

func (s *laneArrivalStats) expectedRepairRunSpan(base uint32, runLen uint8, dataKind transport.Kind, at time.Time) uint8 {
	if runLen == 0 {
		return 0
	}
	if s.repairGroupExists(base, runLen, dataKind) {
		return runLen
	}
	if runLen < maxFECSourceSpan {
		return 0
	}
	for span := uint8(maxFECSourceSpan); span > runLen; span-- {
		if s.groupExists(base, span, dataKind, at) {
			return span
		}
	}
	return runLen
}

func (s *laneArrivalStats) groupExists(base uint32, span uint8, dataKind transport.Kind, at time.Time) bool {
	if s.repairGroupExists(base, span, dataKind) {
		return true
	}
	return s.observedContiguousSpan(base, span, dataKind, at) == span
}

func (s *laneArrivalStats) repairGroupExists(base uint32, span uint8, dataKind transport.Kind) bool {
	group := s.groups[base]
	return group != nil && group.span == span && group.repairKind == otherTransportKind(dataKind)
}

func (s *laneArrivalStats) observedContiguousSpan(base uint32, maxSpan uint8, dataKind transport.Kind, at time.Time) uint8 {
	var span uint8
	for offset := uint32(0); offset < uint32(maxSpan); offset++ {
		arrival, ok := s.data[base+offset]
		if !ok || arrival.kind != dataKind || arrival.at.After(at) {
			break
		}
		span++
	}
	return span
}

func (s *laneArrivalStats) prune(at time.Time) {
	cutoff := at.Add(-qosAccountingRetain)
	for packetID, arrival := range s.data {
		if !arrival.at.IsZero() && arrival.at.Before(cutoff) {
			delete(s.data, packetID)
		}
	}
	for base, group := range s.groups {
		if group != nil && !group.repairAt.IsZero() && group.repairAt.Before(cutoff) {
			delete(s.groups, base)
		}
	}
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

func deltaU64(now, last uint64) uint64 {
	if now > last {
		return now - last
	}
	return 0
}

func lossRatio(expected, arrived uint64) float64 {
	if expected == 0 || arrived >= expected {
		return 0
	}
	return float64(expected-arrived) / float64(expected)
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
