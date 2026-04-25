package send

import "github.com/MeteorsLiu/multipath/internal/debuglog"

type txSLCWindow struct {
	sourceCount int
	pending     []txSymbol
}

type txSymbol struct {
	packetID uint32
	packet   []byte
}

type txRepairGroup struct {
	basePacketID uint32
	packets      [][]byte
}

func newTxSLCWindow(sourceCount int) *txSLCWindow {
	return &txSLCWindow{sourceCount: sourceCount}
}

func (w *txSLCWindow) add(packetID uint32, packet []byte) (txRepairGroup, bool) {
	if w.sourceCount <= 0 {
		debuglog.Printf("send/fec_window", "tx_add_drop invalid_source_count source_count=%d packet_id=%d", w.sourceCount, packetID)
		return txRepairGroup{}, false
	}

	w.pending = append(w.pending, txSymbol{
		packetID: packetID,
		packet:   append([]byte(nil), packet...),
	})
	debuglog.Printf("send/fec_window", "tx_add packet_id=%d bytes=%d pending=%d source_count=%d", packetID, len(packet), len(w.pending), w.sourceCount)

	for len(w.pending) >= w.sourceCount && !w.firstGroupContiguous() {
		debuglog.Printf("send/fec_window", "tx_drop_noncontiguous packet_id=%d pending=%d", w.pending[0].packetID, len(w.pending))
		copy(w.pending, w.pending[1:])
		w.pending = w.pending[:len(w.pending)-1]
	}

	if len(w.pending) < w.sourceCount {
		debuglog.Printf("send/fec_window", "tx_wait pending=%d source_count=%d", len(w.pending), w.sourceCount)
		return txRepairGroup{}, false
	}

	group := txRepairGroup{
		basePacketID: w.pending[0].packetID,
		packets:      make([][]byte, w.sourceCount),
	}
	for i := 0; i < w.sourceCount; i++ {
		group.packets[i] = w.pending[i].packet
	}
	copy(w.pending, w.pending[w.sourceCount:])
	w.pending = w.pending[:len(w.pending)-w.sourceCount]
	debuglog.Printf("send/fec_window", "tx_group base_packet_id=%d shards=%d pending=%d", group.basePacketID, len(group.packets), len(w.pending))
	return group, true
}

func (w *txSLCWindow) firstGroupContiguous() bool {
	base := w.pending[0].packetID
	for i := 1; i < w.sourceCount; i++ {
		if w.pending[i].packetID != base+uint32(i) {
			return false
		}
	}
	return true
}
