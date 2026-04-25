package cfs

import (
	"container/heap"
	"errors"
	"math"
)

const defaultScale = uint64(1024)

var (
	ErrInvalidWeight = errors.New("cfs: weight must be greater than zero")
	ErrQueuedLane    = errors.New("cfs: lane already queued")
)

type Scheduler struct {
	items map[uint8]*item
	queue priorityQueue
	minVR uint64
}

func New() *Scheduler {
	return &Scheduler{
		items: make(map[uint8]*item),
	}
}

func (s *Scheduler) Enqueue(laneID uint8, weight uint32, charge uint32) error {
	if weight == 0 {
		return ErrInvalidWeight
	}

	it, ok := s.items[laneID]
	if !ok {
		it = &item{
			laneID:   laneID,
			weight:   weight,
			vruntime: s.minVR,
			index:    -1,
		}
		s.items[laneID] = it
	}
	if it.index >= 0 {
		return ErrQueuedLane
	}

	it.weight = weight
	if it.vruntime < s.minVR {
		it.vruntime = s.minVR
	}
	s.addCharge(it, charge, weight)
	heap.Push(&s.queue, it)
	return nil
}

func (s *Scheduler) Dequeue() (uint8, bool) {
	if len(s.queue) == 0 {
		return 0, false
	}

	it := heap.Pop(&s.queue).(*item)
	s.minVR = it.vruntime
	return it.laneID, true
}

func (s *Scheduler) addCharge(it *item, charge uint32, weight uint32) {
	if charge == 0 {
		return
	}

	delta := uint64(charge) * defaultScale / uint64(weight)
	if math.MaxUint64-it.vruntime < delta {
		s.rebase(s.minVR)
	}
	it.vruntime += delta
}

func (s *Scheduler) rebase(base uint64) {
	if base == 0 {
		return
	}
	for _, it := range s.items {
		if it.vruntime >= base {
			it.vruntime -= base
		} else {
			it.vruntime = 0
		}
	}
	if s.minVR >= base {
		s.minVR -= base
	} else {
		s.minVR = 0
	}
	heap.Init(&s.queue)
}

type item struct {
	laneID   uint8
	weight   uint32
	vruntime uint64
	index    int
}

type priorityQueue []*item

func (q priorityQueue) Len() int {
	return len(q)
}

func (q priorityQueue) Less(i, j int) bool {
	if q[i].vruntime == q[j].vruntime {
		return q[i].laneID < q[j].laneID
	}
	return q[i].vruntime < q[j].vruntime
}

func (q priorityQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *priorityQueue) Push(x any) {
	it := x.(*item)
	it.index = len(*q)
	*q = append(*q, it)
}

func (q *priorityQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	it.index = -1
	*q = old[:n-1]
	return it
}
