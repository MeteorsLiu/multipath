package recv

import (
	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const (
	defaultRxSLCWindowDataLimit   = 4096
	defaultRxSLCWindowRepairLimit = 1024
)

// rxSLCWindow buffers received DATA shards and REPAIR symbols so a complete
// FEC group with one missing data shard can be reconstructed. Storage uses
// pooled packetbuf.Packet buffers so steady-state operation does not allocate
// per packet.
//
// All methods must be called with the caller's per-session lock held.
type rxSLCWindow struct {
	sourceCount int
	data        map[uint32]*packetbuf.Packet
	repairs     map[uint32]rxRepair
	emitted     map[uint32]bool
	dataOrder   []uint32
	repairOrder []uint32
	maxData     int
	maxRepairs  int
}

type rxRepair struct {
	basePacketID uint32
	key          uint16
	symbol       *packetbuf.Packet
}

// rxRecoverable identifies an FEC group that can be reconstructed. It does
// NOT carry shard slices: shards alias pooled buffers and must therefore be
// rebuilt under the caller's lock immediately before fec.Reconstruct.
type rxRecoverable struct {
	basePacketID uint32
	key          uint16
	missingIndex int
}

func newRxSLCWindow(sourceCount int) *rxSLCWindow {
	return &rxSLCWindow{
		sourceCount: sourceCount,
		data:        make(map[uint32]*packetbuf.Packet),
		repairs:     make(map[uint32]rxRepair),
		emitted:     make(map[uint32]bool),
		maxData:     defaultRxSLCWindowDataLimit,
		maxRepairs:  defaultRxSLCWindowRepairLimit,
	}
}

// storePacket copies bytes into a freshly-pooled Packet sized to fit.
func storePacket(b []byte) *packetbuf.Packet {
	pkt := packetbuf.Acquire(len(b))
	copy(pkt.Payload, b)
	pkt.SetLen(len(b))
	return pkt
}

func (w *rxSLCWindow) addData(packetID uint32, packet []byte) (rxRecoverable, bool) {
	if w.sourceCount <= 0 {
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_data_drop invalid_source_count source_count=%d packet_id=%d", w.sourceCount, packetID)
		}
		return rxRecoverable{}, false
	}
	if existing, ok := w.data[packetID]; ok {
		// Duplicate packet ID; replace and release the prior pooled buffer.
		existing.Release()
	} else {
		w.dataOrder = append(w.dataOrder, packetID)
	}
	w.data[packetID] = storePacket(packet)
	if debuglog.Enabled() {
		debuglog.Printf("recv/fec_window", "rx_data packet_id=%d bytes=%d data=%d repairs=%d", packetID, len(packet), len(w.data), len(w.repairs))
	}

	// Only the (at most sourceCount) repair groups whose basePacketID is in
	// [packetID-sourceCount+1, packetID] can contain packetID. Probe each
	// candidate with an O(1) map lookup instead of scanning every stored
	// repair.
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
		recoverable, ok := w.recoverable(repair)
		if ok {
			w.prune()
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_data_recoverable base_packet_id=%d key=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
			}
			return recoverable, true
		}
		if w.allKnown(repair) {
			repair.symbol.Release()
			delete(w.repairs, base)
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_repair_complete_drop base_packet_id=%d key=%d", repair.basePacketID, repair.key)
			}
		}
	}
	w.prune()
	return rxRecoverable{}, false
}

func (w *rxSLCWindow) addRepair(basePacketID uint32, key uint16, symbol []byte) (rxRecoverable, bool) {
	if w.sourceCount <= 0 {
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_drop invalid_source_count source_count=%d base_packet_id=%d key=%d", w.sourceCount, basePacketID, key)
		}
		return rxRecoverable{}, false
	}
	repair := rxRepair{
		basePacketID: basePacketID,
		key:          key,
		symbol:       storePacket(symbol),
	}
	if w.allKnown(repair) {
		repair.symbol.Release()
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_repair_drop all_known base_packet_id=%d key=%d", basePacketID, key)
		}
		return rxRecoverable{}, false
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
		debuglog.Printf("recv/fec_window", "rx_repair base_packet_id=%d key=%d symbol_len=%d recoverable=%t data=%d repairs=%d", basePacketID, key, len(symbol), ok, len(w.data), len(w.repairs))
	}
	return recoverable, ok
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
	if !ok {
		return nil, false
	}
	if cap(dst) < w.sourceCount+1 {
		dst = make([][]byte, w.sourceCount+1)
	} else {
		dst = dst[:w.sourceCount+1]
		for i := range dst {
			dst[i] = nil
		}
	}
	for i := 0; i < w.sourceCount; i++ {
		if i == r.missingIndex {
			continue
		}
		packetID := r.basePacketID + uint32(i)
		pkt := w.data[packetID]
		if pkt == nil {
			return nil, false
		}
		dst[i] = pkt.Payload
	}
	dst[w.sourceCount] = repair.symbol.Payload
	return dst, true
}

func (w *rxSLCWindow) markEmitted(packetID uint32) bool {
	if w.emitted[packetID] {
		if debuglog.Enabled() {
			debuglog.Printf("recv/fec_window", "rx_emit_skip duplicate packet_id=%d", packetID)
		}
		return false
	}
	w.emitted[packetID] = true
	if debuglog.Enabled() {
		debuglog.Printf("recv/fec_window", "rx_emit_mark packet_id=%d", packetID)
	}
	return true
}

func (w *rxSLCWindow) contains(repair rxRepair, packetID uint32) bool {
	packet := uint64(packetID)
	base := uint64(repair.basePacketID)
	return packet >= base && packet < base+uint64(w.sourceCount)
}

func (w *rxSLCWindow) allKnown(repair rxRepair) bool {
	if w.sourceCount <= 0 {
		return false
	}
	for i := 0; i < w.sourceCount; i++ {
		packetID := repair.basePacketID + uint32(i)
		if w.data[packetID] == nil {
			return false
		}
	}
	return true
}

// recoverable inspects the window and returns identifiers for the repair group
// when exactly one of its data shards is missing.
func (w *rxSLCWindow) recoverable(repair rxRepair) (rxRecoverable, bool) {
	missing := -1
	missingCount := 0
	for i := 0; i < w.sourceCount; i++ {
		packetID := repair.basePacketID + uint32(i)
		if w.data[packetID] == nil {
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
		missingIndex: missing,
	}, true
}

func (w *rxSLCWindow) prune() {
	if w.maxData > 0 {
		for len(w.data) > w.maxData && len(w.dataOrder) > 0 {
			packetID := w.dataOrder[0]
			w.dataOrder = w.dataOrder[1:]
			pkt, ok := w.data[packetID]
			if !ok {
				continue
			}
			delete(w.data, packetID)
			delete(w.emitted, packetID)
			pkt.Release()
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
			if r, ok := w.repairs[basePacketID]; ok {
				r.symbol.Release()
				delete(w.repairs, basePacketID)
			}
			if debuglog.Enabled() {
				debuglog.Printf("recv/fec_window", "rx_prune_repair base_packet_id=%d repairs=%d", basePacketID, len(w.repairs))
			}
			w.dropStaleRepairOrder()
		}
	}
}

// releaseAll returns every pooled buffer held by the window back to its
// packetbuf pool. The window's bookkeeping maps are emptied so the window can
// no longer be used after this call.
func (w *rxSLCWindow) releaseAll() {
	for id, pkt := range w.data {
		pkt.Release()
		delete(w.data, id)
	}
	for id, repair := range w.repairs {
		repair.symbol.Release()
		delete(w.repairs, id)
	}
	w.dataOrder = w.dataOrder[:0]
	w.repairOrder = w.repairOrder[:0]
	for id := range w.emitted {
		delete(w.emitted, id)
	}
}

func (w *rxSLCWindow) dropStaleRepairOrder() {
	for len(w.repairOrder) > 0 {
		if _, ok := w.repairs[w.repairOrder[0]]; ok {
			return
		}
		w.repairOrder = w.repairOrder[1:]
	}
}
