package recv

import "github.com/MeteorsLiu/multipath/internal/packetbuf"

const (
	defaultRxGroupWindowDataLimit   = 4096
	defaultRxGroupWindowGroupLimit  = 1024
	defaultRxGroupWindowClosedLimit = 4096
)

type rxGroupKey struct {
	basePacketID uint32
	sourceSpan   int
}

type rxGroupWindow struct {
	recentData map[uint32]*packetbuf.Packet
	groups     map[rxGroupKey]*rxGroup
	closed     map[rxGroupKey]struct{}

	dataOrder   []uint32
	groupOrder  []rxGroupKey
	closedOrder []rxGroupKey

	maxData   int
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
	group        rxGroupKey
	dataArrived  uint8
	dataExpected uint8
	recovered    bool
	expired      bool
}

func newRxGroupWindow() *rxGroupWindow {
	return &rxGroupWindow{
		recentData: make(map[uint32]*packetbuf.Packet),
		groups:     make(map[rxGroupKey]*rxGroup),
		closed:     make(map[rxGroupKey]struct{}),
		maxData:    defaultRxGroupWindowDataLimit,
		maxGroups:  defaultRxGroupWindowGroupLimit,
		maxClosed:  defaultRxGroupWindowClosedLimit,
	}
}

func storePacket(b []byte) *packetbuf.Packet {
	p := packetbuf.Acquire(len(b))
	copy(p.Payload, b)
	return p
}

func (w *rxGroupWindow) addData(packetID uint32, packet []byte) rxGroupWindowResult {
	if w == nil {
		return rxGroupWindowResult{}
	}

	if old := w.recentData[packetID]; old != nil {
		old.Release()
	} else {
		w.dataOrder = append(w.dataOrder, packetID)
	}
	w.recentData[packetID] = storePacket(packet)

	var out rxGroupWindowResult
	w.eachGroupForPacket(packetID, func(group *rxGroup, index int) {
		group.data[index] = w.recentData[packetID]
		out.add(w.checkGroup(group))
	})
	out.add(w.prune())
	return out
}

func (w *rxGroupWindow) addRepair(basePacketID uint32, key uint16, sourceSpan int, symbol []byte) rxGroupWindowResult {
	if w == nil || sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return rxGroupWindowResult{}
	}

	groupKey := rxGroupKey{basePacketID: basePacketID, sourceSpan: sourceSpan}
	if _, ok := w.closed[groupKey]; ok {
		return rxGroupWindowResult{}
	}

	group := w.groups[groupKey]
	if group == nil {
		group = &rxGroup{
			key:  groupKey,
			data: make([]*packetbuf.Packet, sourceSpan),
		}
		for i := range group.data {
			group.data[i] = w.recentData[basePacketID+uint32(i)]
		}
		w.groups[groupKey] = group
		w.groupOrder = append(w.groupOrder, groupKey)
	}

	if group.hasRepair(key) {
		return w.prune()
	}
	group.repairs = append(group.repairs, rxRepairShard{
		key:    key,
		symbol: storePacket(symbol),
	})

	out := w.checkGroup(group)
	out.add(w.prune())
	return out
}

func (w *rxGroupWindow) buildShardsLocked(r rxGroupRecoverable, shards [][]byte, repairKeys []uint16) ([][]byte, []uint16, bool) {
	if w == nil || r.missingMask == 0 {
		return nil, nil, false
	}

	group := w.groups[r.group]
	if group == nil {
		return nil, nil, false
	}

	missing := countMissing(r.missingMask, r.group.sourceSpan)
	if missing == 0 || len(group.repairs) < missing {
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

	for i, data := range group.data {
		if r.missingMask&(1<<uint(i)) != 0 {
			continue
		}
		if data == nil {
			return nil, nil, false
		}
		shards[i] = data.Payload
	}
	for i := 0; i < missing; i++ {
		repair := group.repairs[i]
		if repair.symbol == nil {
			return nil, nil, false
		}
		shards[r.group.sourceSpan+i] = repair.symbol.Payload
		repairKeys = append(repairKeys, repair.key)
	}
	return shards, repairKeys, true
}

func (w *rxGroupWindow) finishRecovery(r rxGroupRecoverable) rxGroupWindowResult {
	if w == nil {
		return rxGroupWindowResult{}
	}

	group := w.groups[r.group]
	if group == nil {
		return w.prune()
	}

	group.recovered = true
	return w.prune()
}

func (w *rxGroupWindow) expireGroup(key rxGroupKey) rxGroupWindowResult {
	if w == nil {
		return rxGroupWindowResult{}
	}

	group := w.groups[key]
	if group == nil {
		return w.prune()
	}

	out := rxGroupWindowResult{done: []rxGroupDone{w.doneForGroup(group, group.recovered, !group.recovered)}}
	w.closeGroup(group.key)
	w.dropGroup(group.key)
	out.add(w.prune())
	return out
}

func (w *rxGroupWindow) releaseAll() {
	if w == nil {
		return
	}
	for packetID := range w.recentData {
		w.dropData(packetID)
	}
	for key := range w.groups {
		w.dropGroup(key)
	}
	for key := range w.closed {
		delete(w.closed, key)
	}
	w.dataOrder = w.dataOrder[:0]
	w.groupOrder = w.groupOrder[:0]
	w.closedOrder = w.closedOrder[:0]
}

func (w *rxGroupWindow) prune() rxGroupWindowResult {
	var out rxGroupWindowResult

	for w.maxData > 0 && len(w.recentData) > w.maxData && len(w.dataOrder) > 0 {
		packetID := w.dataOrder[0]
		w.dataOrder = w.dataOrder[1:]
		if w.recentData[packetID] == nil {
			continue
		}
		w.eachGroupForPacket(packetID, func(group *rxGroup, index int) {
			out.done = append(out.done, w.doneForGroup(group, group.recovered, !group.recovered))
			w.closeGroup(group.key)
			w.dropGroup(group.key)
		})
		w.dropData(packetID)
	}

	for w.maxGroups > 0 && len(w.groups) > w.maxGroups && len(w.groupOrder) > 0 {
		key := w.groupOrder[0]
		w.groupOrder = w.groupOrder[1:]
		group := w.groups[key]
		if group == nil {
			continue
		}
		out.done = append(out.done, w.doneForGroup(group, group.recovered, !group.recovered))
		w.closeGroup(group.key)
		w.dropGroup(group.key)
	}

	for w.maxClosed > 0 && len(w.closed) > w.maxClosed && len(w.closedOrder) > 0 {
		key := w.closedOrder[0]
		w.closedOrder = w.closedOrder[1:]
		delete(w.closed, key)
	}

	return out
}

func (w *rxGroupWindow) checkGroup(group *rxGroup) rxGroupWindowResult {
	if group == nil {
		return rxGroupWindowResult{}
	}
	if group.recovered {
		return rxGroupWindowResult{}
	}

	missingMask := group.missingMask()
	if missingMask == 0 {
		out := rxGroupWindowResult{done: []rxGroupDone{w.doneForGroup(group, false, false)}}
		w.closeGroup(group.key)
		w.dropGroup(group.key)
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

func (w *rxGroupWindow) eachGroupForPacket(packetID uint32, fn func(*rxGroup, int)) {
	for offset := 0; offset < maxFECSourceSpan; offset++ {
		basePacketID := packetID - uint32(offset)
		for sourceSpan := offset + 1; sourceSpan <= maxFECSourceSpan; sourceSpan++ {
			group := w.groups[rxGroupKey{
				basePacketID: basePacketID,
				sourceSpan:   sourceSpan,
			}]
			if group != nil {
				fn(group, offset)
			}
		}
	}
}

func (w *rxGroupWindow) closeGroup(key rxGroupKey) {
	if _, ok := w.closed[key]; ok {
		return
	}
	w.closed[key] = struct{}{}
	w.closedOrder = append(w.closedOrder, key)
}

func (w *rxGroupWindow) dropData(packetID uint32) {
	data := w.recentData[packetID]
	if data == nil {
		return
	}
	delete(w.recentData, packetID)
	data.Release()
}

func (w *rxGroupWindow) dropGroup(key rxGroupKey) {
	group := w.groups[key]
	if group == nil {
		return
	}
	for i := range group.repairs {
		if group.repairs[i].symbol != nil {
			group.repairs[i].symbol.Release()
		}
	}
	delete(w.groups, key)
}

func (w *rxGroupWindow) doneForGroup(group *rxGroup, recovered, expired bool) rxGroupDone {
	return rxGroupDone{
		group:        group.key,
		dataArrived:  uint8(group.dataArrived()),
		dataExpected: uint8(len(group.data)),
		recovered:    recovered,
		expired:      expired,
	}
}

func (g *rxGroup) hasRepair(key uint16) bool {
	for _, repair := range g.repairs {
		if repair.key == key {
			return true
		}
	}
	return false
}

func (g *rxGroup) dataArrived() int {
	var count int
	for _, data := range g.data {
		if data != nil {
			count++
		}
	}
	return count
}

func (g *rxGroup) missingMask() uint8 {
	var mask uint8
	for i, data := range g.data {
		if data == nil {
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
