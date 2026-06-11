package recv

import "github.com/bits-and-blooms/bitset"

// defaultEmitDedupeWindow is the number of recent packet ids the session-scoped
// emit dedupe tracks behind the highest id seen. It comfortably exceeds the
// rxSLCWindow data horizon so a recovered packet and its late original are
// still recognized as the same id.
const defaultEmitDedupeWindow = 1 << 16

// dedupeBehindThreshold splits "ahead of the window" from "behind / already
// evicted" when interpreting a wrapped uint32 packet-id delta.
const dedupeBehindThreshold = uint32(1) << 31

// emitDedupe tracks which session-scoped packet ids have already been emitted to
// TUN so the original and the FEC-recovered copy of the same packet are written
// at most once. It is session-scoped (packet ids are allocated per session,
// independent of lane) and separate from the FEC receive windows.
//
// It maintains a sliding window [base, base+span) of packet ids over a
// fixed-size bitset ring: bit i corresponds to the packet id n with
// n%span == i, which is unambiguous because the window is exactly span ids
// wide. Ids older than the window are treated as already emitted; an id ahead
// of the window slides base forward and evicts the oldest ids. The bitmap
// storage and operations are delegated to github.com/bits-and-blooms/bitset;
// only the window base and eviction policy are maintained here.
type emitDedupe struct {
	set    *bitset.BitSet
	span   uint32
	base   uint32
	inited bool
}

func newEmitDedupe(span uint32) *emitDedupe {
	if span == 0 {
		span = defaultEmitDedupeWindow
	}
	return &emitDedupe{
		set:  bitset.New(uint(span)),
		span: span,
	}
}

// mark records packetID as emitted and reports whether this is the first time
// it has been seen. It returns true when the caller should emit the packet and
// false for a duplicate (already emitted) or an id older than the tracking
// window (assumed already emitted).
func (d *emitDedupe) mark(packetID uint32) bool {
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

// evict clears the bits for packet ids in [base, newBase) as the window slides
// forward to newBase.
func (d *emitDedupe) evict(newBase uint32) {
	if newBase-d.base >= d.span {
		d.set.ClearAll()
		return
	}
	for e := d.base; e != newBase; e++ {
		d.set.Clear(uint(e % d.span))
	}
}
