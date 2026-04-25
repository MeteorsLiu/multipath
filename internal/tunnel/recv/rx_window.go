package recv

import "github.com/MeteorsLiu/multipath/internal/debuglog"

const (
	defaultRxSLCWindowDataLimit   = 4096
	defaultRxSLCWindowRepairLimit = 1024
)

type rxSLCWindow struct {
	sourceCount int
	data        map[uint32][]byte
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
	symbol       []byte
}

type rxRecoverable struct {
	basePacketID uint32
	key          uint16
	missingIndex int
	shards       [][]byte
}

func newRxSLCWindow(sourceCount int) *rxSLCWindow {
	return &rxSLCWindow{
		sourceCount: sourceCount,
		data:        make(map[uint32][]byte),
		repairs:     make(map[uint32]rxRepair),
		emitted:     make(map[uint32]bool),
		maxData:     defaultRxSLCWindowDataLimit,
		maxRepairs:  defaultRxSLCWindowRepairLimit,
	}
}

func (w *rxSLCWindow) addData(packetID uint32, packet []byte) (rxRecoverable, bool) {
	if w.sourceCount <= 0 {
		debuglog.Printf("recv/fec_window", "rx_data_drop invalid_source_count source_count=%d packet_id=%d", w.sourceCount, packetID)
		return rxRecoverable{}, false
	}
	if _, exists := w.data[packetID]; !exists {
		w.dataOrder = append(w.dataOrder, packetID)
	}
	w.data[packetID] = append([]byte(nil), packet...)
	debuglog.Printf("recv/fec_window", "rx_data packet_id=%d bytes=%d data=%d repairs=%d", packetID, len(packet), len(w.data), len(w.repairs))

	for _, repair := range w.repairs {
		if w.contains(repair, packetID) {
			recoverable, ok := w.recoverable(repair)
			if ok {
				w.prune()
				debuglog.Printf("recv/fec_window", "rx_data_recoverable base_packet_id=%d key=%d missing_index=%d", recoverable.basePacketID, recoverable.key, recoverable.missingIndex)
				return recoverable, true
			}
			if w.allKnown(repair) {
				delete(w.repairs, repair.basePacketID)
				debuglog.Printf("recv/fec_window", "rx_repair_complete_drop base_packet_id=%d key=%d", repair.basePacketID, repair.key)
			}
		}
	}
	w.prune()
	return rxRecoverable{}, false
}

func (w *rxSLCWindow) addRepair(basePacketID uint32, key uint16, symbol []byte) (rxRecoverable, bool) {
	if w.sourceCount <= 0 {
		debuglog.Printf("recv/fec_window", "rx_repair_drop invalid_source_count source_count=%d base_packet_id=%d key=%d", w.sourceCount, basePacketID, key)
		return rxRecoverable{}, false
	}
	repair := rxRepair{
		basePacketID: basePacketID,
		key:          key,
		symbol:       append([]byte(nil), symbol...),
	}
	if w.allKnown(repair) {
		debuglog.Printf("recv/fec_window", "rx_repair_drop all_known base_packet_id=%d key=%d", basePacketID, key)
		return rxRecoverable{}, false
	}
	if _, exists := w.repairs[basePacketID]; !exists {
		w.repairOrder = append(w.repairOrder, basePacketID)
	}
	w.repairs[basePacketID] = repair
	recoverable, ok := w.recoverable(repair)
	w.prune()
	debuglog.Printf("recv/fec_window", "rx_repair base_packet_id=%d key=%d symbol_len=%d recoverable=%t data=%d repairs=%d", basePacketID, key, len(symbol), ok, len(w.data), len(w.repairs))
	return recoverable, ok
}

func (w *rxSLCWindow) markEmitted(packetID uint32) bool {
	if w.emitted[packetID] {
		debuglog.Printf("recv/fec_window", "rx_emit_skip duplicate packet_id=%d", packetID)
		return false
	}
	w.emitted[packetID] = true
	debuglog.Printf("recv/fec_window", "rx_emit_mark packet_id=%d", packetID)
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

func (w *rxSLCWindow) recoverable(repair rxRepair) (rxRecoverable, bool) {
	shards := make([][]byte, w.sourceCount+1)
	missing := -1
	missingCount := 0

	for i := 0; i < w.sourceCount; i++ {
		packetID := repair.basePacketID + uint32(i)
		packet := w.data[packetID]
		if packet == nil {
			missing = i
			missingCount++
			continue
		}
		shards[i] = packet
	}
	if missingCount != 1 {
		return rxRecoverable{}, false
	}
	shards[w.sourceCount] = repair.symbol
	return rxRecoverable{
		basePacketID: repair.basePacketID,
		key:          repair.key,
		missingIndex: missing,
		shards:       shards,
	}, true
}

func (w *rxSLCWindow) prune() {
	if w.maxData > 0 {
		for len(w.data) > w.maxData && len(w.dataOrder) > 0 {
			packetID := w.dataOrder[0]
			w.dataOrder = w.dataOrder[1:]
			if _, ok := w.data[packetID]; !ok {
				continue
			}
			delete(w.data, packetID)
			delete(w.emitted, packetID)
			debuglog.Printf("recv/fec_window", "rx_prune_data packet_id=%d data=%d", packetID, len(w.data))
		}
	}

	w.dropStaleRepairOrder()
	if w.maxRepairs > 0 {
		for len(w.repairs) > w.maxRepairs && len(w.repairOrder) > 0 {
			basePacketID := w.repairOrder[0]
			w.repairOrder = w.repairOrder[1:]
			delete(w.repairs, basePacketID)
			debuglog.Printf("recv/fec_window", "rx_prune_repair base_packet_id=%d repairs=%d", basePacketID, len(w.repairs))
			w.dropStaleRepairOrder()
		}
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
