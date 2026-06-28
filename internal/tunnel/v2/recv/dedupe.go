package recv

import "github.com/bits-and-blooms/bitset"

// defaultPacketIDDedupeWindow is the number of recent packet ids tracked behind
// the highest id seen. It comfortably exceeds the rxGroupWindow data horizon so
// a recovered packet and its late original are still recognized as the same id.
const defaultPacketIDDedupeWindow = 1 << 16

// dedupeBehindThreshold splits "ahead of the window" from "behind / already
// evicted" when interpreting a wrapped uint32 packet-id delta.
const dedupeBehindThreshold = uint32(1) << 31

// packetIDDedupe tracks session-scoped packet ids in a sliding window
// [base, base+span). It is separate from the FEC receive windows.
type packetIDDedupe struct {
	set    *bitset.BitSet
	span   uint32
	base   uint32
	inited bool
}

func newPacketIDDedupe(span uint32) *packetIDDedupe {
	if span == 0 {
		span = defaultPacketIDDedupeWindow
	}
	return &packetIDDedupe{
		set:  bitset.New(uint(span)),
		span: span,
	}
}

// mark records packetID and reports whether this is the first time it has been
// seen in the active window. It returns false for a duplicate or an id older
// than the tracking window.
func (d *packetIDDedupe) mark(packetID uint32) bool {
	if !d.inited {
		// Anchor the first id at the top of a trailing window so reordered
		// earlier ids (up to span-1 below it) still fall inside the window
		// instead of being mistaken for already-evicted ids.
		d.base = packetID - (d.span - 1)
		d.inited = true
	}

	delta := packetID - d.base // uint32 wraparound is intentional
	switch {
	case delta < d.span:
		// Within the current window.
	case delta >= dedupeBehindThreshold:
		// packetID sorts before base: older than the window, assume emitted.
		return false
	default:
		// packetID is ahead of the window; slide base forward so it becomes
		// the newest tracked id, evicting the oldest.
		newBase := packetID - d.span + 1
		d.evict(newBase)
		d.base = newBase
	}

	pos := uint(packetID % d.span)
	if d.set.Test(pos) {
		return false
	}
	d.set.Set(pos)
	return true
}

func (d *packetIDDedupe) seen(packetID uint32) bool {
	if d == nil || !d.inited {
		return false
	}
	delta := packetID - d.base // uint32 wraparound is intentional
	if delta >= d.span {
		return false
	}
	return d.set.Test(uint(packetID % d.span))
}

// evict clears the bits for packet ids in [base, newBase) as the window slides
// forward to newBase.
func (d *packetIDDedupe) evict(newBase uint32) {
	if newBase-d.base >= d.span {
		d.set.ClearAll()
		return
	}
	for e := d.base; e != newBase; e++ {
		d.set.Clear(uint(e % d.span))
	}
}
