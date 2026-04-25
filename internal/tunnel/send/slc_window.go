package send

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
		return txRepairGroup{}, false
	}

	w.pending = append(w.pending, txSymbol{
		packetID: packetID,
		packet:   append([]byte(nil), packet...),
	})

	for len(w.pending) >= w.sourceCount && !w.firstGroupContiguous() {
		copy(w.pending, w.pending[1:])
		w.pending = w.pending[:len(w.pending)-1]
	}

	if len(w.pending) < w.sourceCount {
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
