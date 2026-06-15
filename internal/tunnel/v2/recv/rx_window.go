package recv

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultRxSLCWindowDataLimit   = 4096
	defaultRxSLCWindowRepairLimit = 1024
	defaultRxSLCWindowClosedLimit = 4096
)

// rxSLCWindow buffers received DATA shards and REPAIR symbols so a complete
// FEC group with one missing data shard can be reconstructed. The same group
// facts also drive receive-side QoS samples; there is no separate packet-id
// accounting ledger outside the window.
//
// Storage uses pooled packetbuf.Packet buffers so steady-state operation does
// not allocate per packet.
//
// The window is concerned only with FEC storage, recovery, and group-level QoS
// evidence. Deciding whether a packet has already been emitted to TUN is the
// job of the session-scoped emitDedupe, not the window.
//
// All methods must be called with the caller's per-session lock held.
type rxSLCWindow struct {
	sourceCount int
	data        map[uint32]rxData
	repairs     map[uint32]rxRepair
	closed      map[rxGroupKey]rxClosedGroup
	dataOrder   []uint32
	repairOrder []uint32
	closedOrder []rxGroupKey
	maxData     int
	maxRepairs  int
	maxClosed   int
}

type rxData struct {
	packet *packetbuf.Packet
	kind   transport.Kind
	at     time.Time
	wire   bool
}

type rxRepair struct {
	basePacketID uint32
	key          uint16
	sourceSpan   int
	kind         transport.Kind
	at           time.Time
	symbol       *packetbuf.Packet
}

type rxGroupKey struct {
	basePacketID uint32
	sourceSpan   int
}

type rxClosedGroup struct {
	repairKind   transport.Kind
	repairAt     time.Time
	repairBytes  uint64
	missingIndex int
}

type rxWindowResult struct {
	recoverable    rxRecoverable
	hasRecoverable bool
	sample         qosSample
	hasSample      bool
}

// rxRecoverable identifies an FEC group that can be reconstructed. It does
// NOT carry shard slices: shards alias pooled buffers and must therefore be
// rebuilt under the caller's lock immediately before fec.Reconstruct.
type rxRecoverable struct {
	basePacketID uint32
	key          uint16
	sourceSpan   int
	missingIndex int
}

func newRxSLCWindow(sourceCount int) *rxSLCWindow {
	return &rxSLCWindow{
		sourceCount: sourceCount,
		data:        make(map[uint32]rxData),
		repairs:     make(map[uint32]rxRepair),
		closed:      make(map[rxGroupKey]rxClosedGroup),
		maxData:     defaultRxSLCWindowDataLimit,
		maxRepairs:  defaultRxSLCWindowRepairLimit,
		maxClosed:   defaultRxSLCWindowClosedLimit,
	}
}

// storePacket copies bytes into a freshly-pooled Packet sized to fit.
func storePacket(b []byte) *packetbuf.Packet {
	pkt := packetbuf.Acquire(len(b))
	copy(pkt.Payload, b)
	pkt.SetLen(len(b))
	return pkt
}

func (w *rxSLCWindow) addData(kind transport.Kind, packetID uint32, packet []byte, at time.Time) rxWindowResult {
	if w.sourceCount <= 0 {
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_data_drop invalid_source_count source_count=%d packet_id=%d", w.sourceCount, packetID)
		}
		return rxWindowResult{}
	}
	w.storeData(packetID, packet, kind, at, true)
	if debuglog.Enabled() {
		debuglog.Printf("recv/fec_window", "rx_data packet_id=%d bytes=%d data=%d repairs=%d", packetID, len(packet), len(w.data), len(w.repairs))
	}

	var out rxWindowResult
	// Only the (at most sourceCount) repair groups whose basePacketID is in
	// [packetID-sourceCount+1, packetID] can contain packetID. Probe each
	// candidate with an O(1) map lookup instead of scanning every stored repair.
	for offset := 0; offset < w.sourceCount; offset++ {
		base := packetID - uint32(offset)
		repair, ok := w.repairs[base]
		if !ok {
			continue
		}
		if !w.contains(repair, packetID) {
			// Defensive: handles the unlikely uint32 wraparound where the
			// stored basePacketID is not actually contiguous with packetID.
			continue
		}
		if w.isClosed(repair) {
			w.dropRepair(base)
			continue
		}
		if sample, ok := w.completeGroupSample(repair, at); ok {
			w.closeGroup(repair, -1)
			w.dropRepair(base)
			if !out.hasSample {
				out.sample = sample
				out.hasSample = true
			}
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_group_complete base_packet_id=%d key=%d source_span=%d", repair.basePacketID, repair.key, repair.sourceSpan)
			}
			continue
		}
		recoverable, ok := w.recoverable(repair)
		if ok && !out.hasRecoverable {
			out.recoverable = recoverable
			out.hasRecoverable = true
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_data_recoverable base_packet_id=%d key=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
			}
		}
		if w.allKnown(repair) {
			w.dropRepair(base)
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_repair_complete_drop base_packet_id=%d key=%d", repair.basePacketID, repair.key)
			}
		}
	}
	w.prune()
	return out
}

func (w *rxSLCWindow) addRepair(kind transport.Kind, basePacketID uint32, key uint16, sourceSpan int, symbol []byte, at time.Time) rxWindowResult {
	if w.sourceCount <= 0 || sourceSpan <= 0 || sourceSpan > w.sourceCount {
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_drop invalid_source_span source_count=%d source_span=%d base_packet_id=%d key=%d", w.sourceCount, sourceSpan, basePacketID, key)
		}
		return rxWindowResult{}
	}
	repair := rxRepair{
		basePacketID: basePacketID,
		key:          key,
		sourceSpan:   sourceSpan,
		kind:         kind,
		at:           at,
		symbol:       storePacket(symbol),
	}
	if w.isClosed(repair) {
		repair.symbol.Release()
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_drop closed_group base_packet_id=%d key=%d source_span=%d", basePacketID, key, sourceSpan)
		}
		return rxWindowResult{}
	}
	if sample, ok := w.completeGroupSample(repair, at); ok {
		repair.symbol.Release()
		w.closeGroup(repair, -1)
		w.prune()
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_complete base_packet_id=%d key=%d source_span=%d symbol_len=%d", basePacketID, key, sourceSpan, len(symbol))
		}
		return rxWindowResult{sample: sample, hasSample: true}
	}
	if w.allKnown(repair) {
		repair.symbol.Release()
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_drop all_known base_packet_id=%d key=%d source_span=%d", basePacketID, key, sourceSpan)
		}
		return rxWindowResult{}
	}
	if existing, ok := w.repairs[basePacketID]; ok {
		existing.symbol.Release()
	} else {
		w.repairOrder = append(w.repairOrder, basePacketID)
	}
	w.repairs[basePacketID] = repair
	recoverable, ok := w.recoverable(repair)
	w.prune()
	if debuglog.Enabled() {
		debuglog.Printf("recv/fec_window", "rx_repair base_packet_id=%d key=%d source_span=%d symbol_len=%d recoverable=%t data=%d repairs=%d", basePacketID, key, sourceSpan, len(symbol), ok, len(w.data), len(w.repairs))
	}
	if ok {
		return rxWindowResult{recoverable: recoverable, hasRecoverable: true}
	}
	return rxWindowResult{}
}

func (w *rxSLCWindow) observeLateData(kind transport.Kind, packetID uint32, packet []byte, at time.Time) rxWindowResult {
	if w.sourceCount <= 0 || !qosKnownKind(kind) {
		return rxWindowResult{}
	}
	existing, ok := w.data[packetID]
	if !ok || existing.packet == nil || existing.wire {
		return rxWindowResult{}
	}
	w.storeData(packetID, packet, kind, at, true)
	sample, ok := w.lateClosedGroupSample(packetID, at)
	w.prune()
	if !ok {
		return rxWindowResult{}
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv/fec_window", "rx_late_data_sample packet_id=%d kind=%d", packetID, kind)
	}
	return rxWindowResult{sample: sample, hasSample: true}
}

func (w *rxSLCWindow) finishRecovery(r rxRecoverable, recovered []byte, at time.Time) (qosSample, bool) {
	packetID := r.basePacketID + uint32(r.missingIndex)
	sample, ok := w.recoveredGroupSample(r, uint64(len(recovered)), at)
	w.storeData(packetID, recovered, 0, at, false)
	if repair, exists := w.repairs[r.basePacketID]; exists {
		w.closeGroup(repair, r.missingIndex)
	}
	w.dropRepair(r.basePacketID)
	w.prune()
	return sample, ok
}

// buildShardsLocked materializes the shard slices for a recoverable group from
// the current window state. Caller MUST hold the per-session lock and use the
// returned slices before any subsequent addData/addRepair/prune call.
//
// Returns false if the window no longer contains the expected shards (for
// example because of a concurrent prune); callers must abort recovery in that
// case.
func (w *rxSLCWindow) buildShardsLocked(r rxRecoverable, dst [][]byte) ([][]byte, bool) {
	repair, ok := w.repairs[r.basePacketID]
	if !ok || w.isClosed(repair) {
		return nil, false
	}
	sourceSpan := r.sourceSpan
	if sourceSpan <= 0 || sourceSpan > w.sourceCount || repair.sourceSpan != sourceSpan {
		return nil, false
	}
	if cap(dst) < sourceSpan+1 {
		dst = make([][]byte, sourceSpan+1)
	} else {
		dst = dst[:sourceSpan+1]
		for i := range dst {
			dst[i] = nil
		}
	}
	for i := 0; i < sourceSpan; i++ {
		if i == r.missingIndex {
			continue
		}
		packetID := r.basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil {
			return nil, false
		}
		dst[i] = data.packet.Payload
	}
	dst[sourceSpan] = repair.symbol.Payload
	return dst, true
}

func (w *rxSLCWindow) storeData(packetID uint32, packet []byte, kind transport.Kind, at time.Time, wire bool) {
	if existing, ok := w.data[packetID]; ok {
		// Duplicate packet ID; replace and release the prior pooled buffer.
		existing.packet.Release()
	} else {
		w.dataOrder = append(w.dataOrder, packetID)
	}
	w.data[packetID] = rxData{
		packet: storePacket(packet),
		kind:   kind,
		at:     at,
		wire:   wire,
	}
}

func (w *rxSLCWindow) contains(repair rxRepair, packetID uint32) bool {
	packet := uint64(packetID)
	base := uint64(repair.basePacketID)
	return packet >= base && packet < base+uint64(repair.sourceSpan)
}

func (w *rxSLCWindow) allKnown(repair rxRepair) bool {
	if repair.sourceSpan <= 0 || repair.sourceSpan > w.sourceCount {
		return false
	}
	for i := 0; i < repair.sourceSpan; i++ {
		packetID := repair.basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil {
			return false
		}
	}
	return true
}

func (w *rxSLCWindow) completeGroupSample(repair rxRepair, at time.Time) (qosSample, bool) {
	if repair.sourceSpan <= 0 || repair.sourceSpan > w.sourceCount || w.isClosed(repair) || !qosKnownKind(repair.kind) {
		return qosSample{}, false
	}
	var (
		dataKind  transport.Kind
		firstAt   time.Time
		dataBytes uint64
	)
	for i := 0; i < repair.sourceSpan; i++ {
		packetID := repair.basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil || !data.wire || !qosKnownKind(data.kind) {
			return qosSample{}, false
		}
		if dataKind == 0 {
			dataKind = data.kind
		} else if dataKind != data.kind {
			return qosSample{}, false
		}
		firstAt = earliest(firstAt, data.at)
		dataBytes += uint64(len(data.packet.Payload))
	}
	firstAt = earliest(firstAt, repair.at)
	return qosSample{
		At:           at,
		Duration:     sampleDuration(firstAt, at),
		DataKind:     dataKind,
		DataArrived:  uint64(repair.sourceSpan),
		DataExpected: uint64(repair.sourceSpan),
		DataBytes:    dataBytes,
		RepairKind:   repair.kind,
		RepairBytes:  uint64(len(repair.symbol.Payload)),
	}, true
}

func (w *rxSLCWindow) recoveredGroupSample(r rxRecoverable, recoveredBytes uint64, at time.Time) (qosSample, bool) {
	if recoveredBytes == 0 || r.sourceSpan <= 0 || r.sourceSpan > w.sourceCount {
		return qosSample{}, false
	}
	repair, ok := w.repairs[r.basePacketID]
	if !ok || w.isClosed(repair) || repair.sourceSpan != r.sourceSpan || !qosKnownKind(repair.kind) {
		return qosSample{}, false
	}
	var (
		dataKind    transport.Kind
		firstAt     = repair.at
		dataBytes   uint64
		dataArrived uint64
	)
	for i := 0; i < r.sourceSpan; i++ {
		if i == r.missingIndex {
			continue
		}
		packetID := r.basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil || !data.wire || !qosKnownKind(data.kind) {
			return qosSample{}, false
		}
		if dataKind == 0 {
			dataKind = data.kind
		} else if dataKind != data.kind {
			return qosSample{}, false
		}
		firstAt = earliest(firstAt, data.at)
		dataBytes += uint64(len(data.packet.Payload))
		dataArrived++
	}
	if dataKind == 0 {
		dataKind = otherTransportKind(repair.kind)
	}
	if dataKind == transport.KindTCP && repair.kind == transport.KindUDP && dataArrived < uint64(r.sourceSpan) {
		return qosSample{}, false
	}
	return qosSample{
		At:             at,
		Duration:       sampleDuration(firstAt, at),
		DataKind:       dataKind,
		DataArrived:    dataArrived,
		DataExpected:   uint64(r.sourceSpan),
		DataBytes:      dataBytes,
		RecoveredBytes: recoveredBytes,
		RepairKind:     repair.kind,
		RepairBytes:    uint64(len(repair.symbol.Payload)),
	}, true
}

func (w *rxSLCWindow) lateClosedGroupSample(packetID uint32, at time.Time) (qosSample, bool) {
	for offset := 0; offset < w.sourceCount; offset++ {
		if packetID < uint32(offset) {
			break
		}
		base := packetID - uint32(offset)
		for sourceSpan := offset + 1; sourceSpan <= w.sourceCount; sourceSpan++ {
			key := rxGroupKey{basePacketID: base, sourceSpan: sourceSpan}
			closed, ok := w.closed[key]
			if !ok || closed.missingIndex != offset || !qosKnownKind(closed.repairKind) {
				continue
			}
			sample, ok := w.closedCompleteGroupSample(base, sourceSpan, closed, at)
			if ok {
				return sample, true
			}
		}
	}
	return qosSample{}, false
}

func (w *rxSLCWindow) closedCompleteGroupSample(basePacketID uint32, sourceSpan int, closed rxClosedGroup, at time.Time) (qosSample, bool) {
	if sourceSpan <= 0 || sourceSpan > w.sourceCount {
		return qosSample{}, false
	}
	var (
		dataKind  transport.Kind
		firstAt   = closed.repairAt
		dataBytes uint64
	)
	for i := 0; i < sourceSpan; i++ {
		packetID := basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil || !data.wire || !qosKnownKind(data.kind) {
			return qosSample{}, false
		}
		if dataKind == 0 {
			dataKind = data.kind
		} else if dataKind != data.kind {
			return qosSample{}, false
		}
		firstAt = earliest(firstAt, data.at)
		dataBytes += uint64(len(data.packet.Payload))
	}
	return qosSample{
		At:           at,
		Duration:     sampleDuration(firstAt, at),
		DataKind:     dataKind,
		DataArrived:  uint64(sourceSpan),
		DataExpected: uint64(sourceSpan),
		DataBytes:    dataBytes,
		RepairKind:   closed.repairKind,
		RepairBytes:  closed.repairBytes,
	}, true
}

// recoverable inspects the window and returns identifiers for the repair group
// when exactly one of its data shards is missing.
func (w *rxSLCWindow) recoverable(repair rxRepair) (rxRecoverable, bool) {
	missing := -1
	missingCount := 0
	if repair.sourceSpan <= 0 || repair.sourceSpan > w.sourceCount || w.isClosed(repair) {
		return rxRecoverable{}, false
	}
	for i := 0; i < repair.sourceSpan; i++ {
		packetID := repair.basePacketID + uint32(i)
		data, ok := w.data[packetID]
		if !ok || data.packet == nil {
			missing = i
			missingCount++
		}
	}
	if missingCount != 1 {
		return rxRecoverable{}, false
	}
	return rxRecoverable{
		basePacketID: repair.basePacketID,
		key:          repair.key,
		sourceSpan:   repair.sourceSpan,
		missingIndex: missing,
	}, true
}

func (w *rxSLCWindow) groupKey(repair rxRepair) rxGroupKey {
	return rxGroupKey{basePacketID: repair.basePacketID, sourceSpan: repair.sourceSpan}
}

func (w *rxSLCWindow) isClosed(repair rxRepair) bool {
	_, ok := w.closed[w.groupKey(repair)]
	return ok
}

func (w *rxSLCWindow) closeGroup(repair rxRepair, missingIndex int) {
	key := w.groupKey(repair)
	if _, ok := w.closed[key]; ok {
		return
	}
	w.closed[key] = rxClosedGroup{
		repairKind:   repair.kind,
		repairAt:     repair.at,
		repairBytes:  uint64(len(repair.symbol.Payload)),
		missingIndex: missingIndex,
	}
	w.closedOrder = append(w.closedOrder, key)
}

func (w *rxSLCWindow) dropRepair(basePacketID uint32) {
	repair, ok := w.repairs[basePacketID]
	if !ok {
		return
	}
	repair.symbol.Release()
	delete(w.repairs, basePacketID)
}

func (w *rxSLCWindow) prune() {
	if w.maxData > 0 {
		for len(w.data) > w.maxData && len(w.dataOrder) > 0 {
			packetID := w.dataOrder[0]
			w.dataOrder = w.dataOrder[1:]
			data, ok := w.data[packetID]
			if !ok {
				continue
			}
			delete(w.data, packetID)
			data.packet.Release()
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_prune_data packet_id=%d data=%d", packetID, len(w.data))
			}
		}
	}

	w.dropStaleRepairOrder()
	if w.maxRepairs > 0 {
		for len(w.repairs) > w.maxRepairs && len(w.repairOrder) > 0 {
			basePacketID := w.repairOrder[0]
			w.repairOrder = w.repairOrder[1:]
			w.dropRepair(basePacketID)
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_prune_repair base_packet_id=%d repairs=%d", basePacketID, len(w.repairs))
			}
			w.dropStaleRepairOrder()
		}
	}

	if w.maxClosed > 0 {
		for len(w.closed) > w.maxClosed && len(w.closedOrder) > 0 {
			key := w.closedOrder[0]
			w.closedOrder = w.closedOrder[1:]
			delete(w.closed, key)
		}
	}
}

// releaseAll returns every pooled buffer held by the window back to its
// packetbuf pool. The window's bookkeeping maps are emptied so the window can
// no longer be used after this call.
func (w *rxSLCWindow) releaseAll() {
	for id, data := range w.data {
		data.packet.Release()
		delete(w.data, id)
	}
	for id, repair := range w.repairs {
		repair.symbol.Release()
		delete(w.repairs, id)
	}
	for key := range w.closed {
		delete(w.closed, key)
	}
	w.dataOrder = w.dataOrder[:0]
	w.repairOrder = w.repairOrder[:0]
	w.closedOrder = w.closedOrder[:0]
}

func (w *rxSLCWindow) dropStaleRepairOrder() {
	for len(w.repairOrder) > 0 {
		if _, ok := w.repairs[w.repairOrder[0]]; ok {
			return
		}
		w.repairOrder = w.repairOrder[1:]
	}
}

func earliest(current, candidate time.Time) time.Time {
	if candidate.IsZero() {
		return current
	}
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

func sampleDuration(first, at time.Time) time.Duration {
	if first.IsZero() || at.IsZero() {
		return 0
	}
	d := at.Sub(first)
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}
