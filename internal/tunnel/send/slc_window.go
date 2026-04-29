package send

import (
	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

type txSLCWindow struct {
	sourceCount int
	pending     []txSymbol
}

type txSymbol struct {
	packetID uint32
	packet   *packetbuf.Packet
}

// txRepairGroup carries the data shards for one FEC group. The consumer takes
// ownership of the packets and MUST call Release on each entry once the group
// is no longer needed (typically after fec.Encode and the REPAIR frame is
// enqueued).
type txRepairGroup struct {
	basePacketID uint32
	sourceSpan   uint8
	packets      []*packetbuf.Packet
}

func newTxSLCWindow(sourceCount int) *txSLCWindow {
	return &txSLCWindow{sourceCount: sourceCount}
}

func (w *txSLCWindow) add(packetID uint32, packet []byte) (txRepairGroup, bool) {
	if w.sourceCount <= 0 {
		if debuglog.Enabled() {
			debuglog.Printf("send/fec_window", "tx_add_drop invalid_source_count source_count=%d packet_id=%d", w.sourceCount, packetID)
		}
		return txRepairGroup{}, false
	}

	pkt := packetbuf.Acquire(len(packet))
	copy(pkt.Payload, packet)
	pkt.SetLen(len(packet))
	w.pending = append(w.pending, txSymbol{
		packetID: packetID,
		packet:   pkt,
	})
	if debuglog.Enabled() {
		debuglog.Printf("send/fec_window", "tx_add packet_id=%d bytes=%d pending=%d source_count=%d", packetID, len(packet), len(w.pending), w.sourceCount)
	}

	for len(w.pending) >= w.sourceCount && !w.firstGroupContiguous() {
		if debuglog.Enabled() {
			debuglog.Printf("send/fec_window", "tx_drop_noncontiguous packet_id=%d pending=%d", w.pending[0].packetID, len(w.pending))
		}
		w.pending[0].packet.Release()
		copy(w.pending, w.pending[1:])
		w.pending = w.pending[:len(w.pending)-1]
	}

	if len(w.pending) < w.sourceCount {
		if debuglog.Enabled() {
			debuglog.Printf("send/fec_window", "tx_wait pending=%d source_count=%d", len(w.pending), w.sourceCount)
		}
		return txRepairGroup{}, false
	}

	group := txRepairGroup{
		basePacketID: w.pending[0].packetID,
		sourceSpan:   uint8(w.sourceCount),
		packets:      make([]*packetbuf.Packet, w.sourceCount),
	}
	for i := 0; i < w.sourceCount; i++ {
		group.packets[i] = w.pending[i].packet
	}
	copy(w.pending, w.pending[w.sourceCount:])
	w.pending = w.pending[:len(w.pending)-w.sourceCount]
	if debuglog.Enabled() {
		debuglog.Printf("send/fec_window", "tx_group base_packet_id=%d shards=%d pending=%d", group.basePacketID, len(group.packets), len(w.pending))
	}
	return group, true
}

func (w *txSLCWindow) flush() (txRepairGroup, bool) {
	if w.sourceCount <= 0 || len(w.pending) == 0 {
		return txRepairGroup{}, false
	}

	count := contiguousPendingPrefix(w.pending, w.sourceCount)
	if count == 0 {
		return txRepairGroup{}, false
	}
	group := txRepairGroup{
		basePacketID: w.pending[0].packetID,
		sourceSpan:   uint8(count),
		packets:      make([]*packetbuf.Packet, count),
	}
	for i := 0; i < count; i++ {
		group.packets[i] = w.pending[i].packet
	}
	copy(w.pending, w.pending[count:])
	w.pending = w.pending[:len(w.pending)-count]
	if debuglog.Enabled() {
		debuglog.Printf("send/fec_window", "tx_flush base_packet_id=%d shards=%d pending=%d", group.basePacketID, len(group.packets), len(w.pending))
	}
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

func contiguousPendingPrefix(pending []txSymbol, max int) int {
	if len(pending) == 0 || max <= 0 {
		return 0
	}
	if max > len(pending) {
		max = len(pending)
	}
	base := pending[0].packetID
	for i := 1; i < max; i++ {
		if pending[i].packetID != base+uint32(i) {
			return i
		}
	}
	return max
}

// releaseAll returns every pooled buffer still in pending back to the
// packetbuf pool. Used during session close so leftover shards do not leak.
func (w *txSLCWindow) releaseAll() {
	for i := range w.pending {
		w.pending[i].packet.Release()
	}
	w.pending = w.pending[:0]
}
