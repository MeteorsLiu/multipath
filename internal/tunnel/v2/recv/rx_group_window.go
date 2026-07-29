package recv

import (
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/emirpasic/gods/v2/trees/redblacktree"
)

const (
	// At four 1440-byte DATA packets per group, this covers about 1.9 seconds at
	// 200 Mbit/s on one lane.
	defaultRxGroupWindowGroupLimit  = 32 * 1024 / maxFECSourceSpan
	defaultRxGroupWindowClosedLimit = 32 * 1024
)

type rxGroupKey struct {
	groupID    uint32
	sourceSpan int
}

func compareGroupID(a, b uint32) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type rxGroupWindow struct {
	groups *redblacktree.Tree[uint32, *rxGroup]
	closed *redblacktree.Tree[uint32, struct{}]

	maxGroups int
	maxClosed int
}

type rxGroup struct {
	key       rxGroupKey
	data      []*packetbuf.Packet
	repairs   []rxRepairShard
	recovered bool
}

type rxRepairShard struct {
	key    uint16
	symbol *packetbuf.Packet
}

type rxGroupWindowResult struct {
	recoverable []rxGroupRecoverable
	done        []rxGroupDone
}

type rxGroupRecoverable struct {
	group       rxGroupKey
	missingMask uint8
}

type rxGroupDone struct {
	group          rxGroupKey
	dataArrived    uint8
	dataExpected   uint8
	expectedBytes  uint64
	maxSourceBytes uint64
	recovered      bool
	expired        bool
}

func newRxGroupWindow() *rxGroupWindow {
	return &rxGroupWindow{
		groups:    redblacktree.NewWith[uint32, *rxGroup](compareGroupID),
		closed:    redblacktree.NewWith[uint32, struct{}](compareGroupID),
		maxGroups: defaultRxGroupWindowGroupLimit,
		maxClosed: defaultRxGroupWindowClosedLimit,
	}
}

func storePacket(b []byte) *packetbuf.Packet {
	p := packetbuf.Acquire(len(b))
	copy(p.Payload, b)
	return p
}

func (w *rxGroupWindow) addData(groupID uint32, sourceIndex uint8, packet []byte) (rxGroupWindowResult, bool) {
	group, _ := w.groups.Get(groupID)
	if group == nil {
		if _, ok := w.closed.Get(groupID); ok {
			return rxGroupWindowResult{}, false
		}
		group = &rxGroup{
			key:  rxGroupKey{groupID: groupID},
			data: make([]*packetbuf.Packet, maxFECSourceSpan),
		}
		w.groups.Put(groupID, group)
	}
	if group.key.sourceSpan > 0 && int(sourceIndex) >= group.key.sourceSpan {
		return w.prune(), false
	}

	if old := group.data[sourceIndex]; old != nil {
		old.Release()
	}
	group.data[sourceIndex] = storePacket(packet)

	var out rxGroupWindowResult
	if group.key.sourceSpan > 0 {
		out.add(w.checkGroup(group))
	}
	out.add(w.prune())
	return out, true
}

func (w *rxGroupWindow) addRepair(groupID uint32, key uint16, sourceSpan int, symbol []byte) (rxGroupWindowResult, bool) {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return rxGroupWindowResult{}, false
	}

	group, _ := w.groups.Get(groupID)
	if group == nil {
		if _, ok := w.closed.Get(groupID); ok {
			return rxGroupWindowResult{}, false
		}
		group = &rxGroup{
			key:  rxGroupKey{groupID: groupID, sourceSpan: sourceSpan},
			data: make([]*packetbuf.Packet, maxFECSourceSpan),
		}
		w.groups.Put(groupID, group)
	} else if !group.acceptsSourceSpan(sourceSpan) {
		return w.prune(), false
	} else if group.key.sourceSpan == 0 {
		group.key.sourceSpan = sourceSpan
	}

	if group.hasRepair(key) {
		return w.prune(), true
	}
	group.repairs = append(group.repairs, rxRepairShard{
		key:    key,
		symbol: storePacket(symbol),
	})

	out := w.checkGroup(group)
	out.add(w.prune())
	return out, true
}

func (w *rxGroupWindow) buildShardsLocked(r rxGroupRecoverable, shards [][]byte, repairKeys []uint16) ([][]byte, []uint16, bool) {
	if r.missingMask == 0 {
		return nil, nil, false
	}

	group, _ := w.groups.Get(r.group.groupID)
	if group == nil || group.key.sourceSpan != r.group.sourceSpan {
		return nil, nil, false
	}

	missing := countMissing(r.missingMask, r.group.sourceSpan)
	if len(group.repairs) < missing {
		return nil, nil, false
	}

	need := r.group.sourceSpan + missing
	if cap(shards) < need {
		shards = make([][]byte, need)
	} else {
		shards = shards[:need]
		for i := range shards {
			shards[i] = nil
		}
	}
	if cap(repairKeys) < missing {
		repairKeys = make([]uint16, 0, missing)
	} else {
		repairKeys = repairKeys[:0]
	}

	for i := 0; i < r.group.sourceSpan; i++ {
		if r.missingMask&(1<<uint(i)) != 0 {
			continue
		}
		data := group.data[i]
		if data == nil {
			return nil, nil, false
		}
		shards[i] = data.Payload
	}
	for i := 0; i < missing; i++ {
		repair := group.repairs[i]
		shards[r.group.sourceSpan+i] = repair.symbol.Payload
		repairKeys = append(repairKeys, repair.key)
	}
	return shards, repairKeys, true
}

func (w *rxGroupWindow) finishRecovery(r rxGroupRecoverable, recoveredBytes, recoveredMaxBytes uint64) rxGroupWindowResult {
	group, _ := w.groups.Get(r.group.groupID)
	if group == nil {
		return w.prune()
	}

	group.recovered = true
	out := rxGroupWindowResult{done: []rxGroupDone{w.doneForGroup(group, true, false, recoveredBytes, recoveredMaxBytes)}}
	w.closeGroup(group.key.groupID)
	w.dropGroup(group.key.groupID)
	out.add(w.prune())
	return out
}

func (w *rxGroupWindow) expireGroup(groupID uint32) rxGroupWindowResult {
	group, _ := w.groups.Get(groupID)
	if group == nil {
		return w.prune()
	}
	if group.key.sourceSpan == 0 {
		w.closeGroup(groupID)
		w.dropGroup(groupID)
		return w.prune()
	}

	out := w.checkGroup(group)
	if len(out.done) > 0 || len(out.recoverable) > 0 {
		out.add(w.prune())
		return out
	}

	out = rxGroupWindowResult{done: []rxGroupDone{w.doneForGroup(group, group.recovered, !group.recovered, 0, 0)}}
	w.closeGroup(groupID)
	w.dropGroup(groupID)
	out.add(w.prune())
	return out
}

func (w *rxGroupWindow) releaseAll() {
	for !w.groups.Empty() {
		w.dropGroup(w.groups.Left().Key)
	}
	w.closed.Clear()
}

func (w *rxGroupWindow) prune() rxGroupWindowResult {
	var out rxGroupWindowResult

	for w.maxGroups > 0 && w.groups.Size() > w.maxGroups {
		group := w.groups.Left().Value
		if group.key.sourceSpan > 0 {
			out.done = append(out.done, w.doneForGroup(group, group.recovered, !group.recovered, 0, 0))
		}
		w.closeGroup(group.key.groupID)
		w.dropGroup(group.key.groupID)
	}

	for w.maxClosed > 0 && w.closed.Size() > w.maxClosed {
		w.closed.Remove(w.closed.Left().Key)
	}

	return out
}

func (w *rxGroupWindow) checkGroup(group *rxGroup) rxGroupWindowResult {
	if group.recovered || group.key.sourceSpan <= 0 {
		return rxGroupWindowResult{}
	}

	missingMask := group.missingMask()
	if missingMask == 0 {
		out := rxGroupWindowResult{done: []rxGroupDone{w.doneForGroup(group, false, false, 0, 0)}}
		w.closeGroup(group.key.groupID)
		w.dropGroup(group.key.groupID)
		return out
	}

	missing := countMissing(missingMask, group.key.sourceSpan)
	if missing <= len(group.repairs) {
		return rxGroupWindowResult{recoverable: []rxGroupRecoverable{{
			group:       group.key,
			missingMask: missingMask,
		}}}
	}
	return rxGroupWindowResult{}
}

func (w *rxGroupWindow) closeGroup(groupID uint32) {
	if _, ok := w.closed.Get(groupID); ok {
		return
	}
	w.closed.Put(groupID, struct{}{})
}

func (w *rxGroupWindow) dropGroup(groupID uint32) {
	group, _ := w.groups.Get(groupID)
	if group == nil {
		return
	}
	for i, data := range group.data {
		if data != nil {
			data.Release()
			group.data[i] = nil
		}
	}
	for i := range group.repairs {
		group.repairs[i].symbol.Release()
		group.repairs[i].symbol = nil
	}
	w.groups.Remove(groupID)
}

func (w *rxGroupWindow) doneForGroup(group *rxGroup, recovered, expired bool, recoveredBytes, recoveredMaxBytes uint64) rxGroupDone {
	done := rxGroupDone{
		group:          group.key,
		dataExpected:   uint8(group.key.sourceSpan),
		maxSourceBytes: recoveredMaxBytes,
		recovered:      recovered,
		expired:        expired,
	}
	expectedBytes := recoveredBytes
	for i := 0; i < group.key.sourceSpan; i++ {
		data := group.data[i]
		if data != nil {
			done.dataArrived++
			dataBytes := uint64(len(data.Payload))
			expectedBytes += dataBytes
			if dataBytes > done.maxSourceBytes {
				done.maxSourceBytes = dataBytes
			}
		}
	}
	done.expectedBytes = expectedBytes
	return done
}

func (g *rxGroup) hasRepair(key uint16) bool {
	for _, repair := range g.repairs {
		if repair.key == key {
			return true
		}
	}
	return false
}

func (g *rxGroup) acceptsSourceSpan(sourceSpan int) bool {
	if sourceSpan <= 0 || sourceSpan > len(g.data) {
		return false
	}
	if g.key.sourceSpan > 0 {
		return g.key.sourceSpan == sourceSpan
	}
	for i := sourceSpan; i < len(g.data); i++ {
		if g.data[i] != nil {
			return false
		}
	}
	return true
}

func (g *rxGroup) missingMask() uint8 {
	var mask uint8
	for i := 0; i < g.key.sourceSpan; i++ {
		if g.data[i] == nil {
			mask |= 1 << uint(i)
		}
	}
	return mask
}

func (r *rxGroupWindowResult) add(other rxGroupWindowResult) {
	r.recoverable = append(r.recoverable, other.recoverable...)
	r.done = append(r.done, other.done...)
}

func countMissing(mask uint8, sourceSpan int) int {
	var count int
	for i := 0; i < sourceSpan; i++ {
		if mask&(1<<uint(i)) != 0 {
			count++
		}
	}
	return count
}

func firstMissingIndex(mask uint8, sourceSpan int) int {
	for i := 0; i < sourceSpan; i++ {
		if mask&(1<<uint(i)) != 0 {
			return i
		}
	}
	return -1
}
