package cfs

import (
	"container/heap"
	"fmt"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
	"github.com/prometheus/client_golang/prometheus"
)

type pathHeap []*cfsPath

func (h pathHeap) Len() int           { return len(h) }
func (h pathHeap) Less(i, j int) bool { return h[i].virtualSent < h[j].virtualSent }
func (h pathHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *pathHeap) Push(x any) {
	n := len(*h)
	item := x.(*cfsPath)
	item.heapIdx = n
	*h = append(*h, item)
}

func (h *pathHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil    // don't stop the GC from reclaiming the item eventually
	item.heapIdx = -1 // for safety
	*h = old[0 : n-1]
	return item
}

type schedulerImpl struct {
	mu           sync.Mutex
	heap         pathHeap
	isServerSide bool
}

func NewCFSScheduler(isServerSide bool) scheduler.Scheduler {
	return &schedulerImpl{isServerSide: isServerSide}
}

func (s *schedulerImpl) AddPath(path scheduler.SchedulablePath) {
	fmt.Println("push", path.String())
	cPath, ok := path.(*cfsPath)
	if !ok {
		panic("invalid path underlying type")
	}
	prom.NodeConnInPool.With(prometheus.Labels{"addr": path.String()}).Inc()

	s.mu.Lock()
	if s.heap.Len() > 0 && s.heap[0].virtualSent > 0 {
		minVirtualSent := s.heap[0].virtualSent

		if minVirtualSent > 1500 {
			minVirtualSent -= 1500 // max allow 1 packet
		}
		cPath.setVirtualSent(minVirtualSent)
	}
	heap.Push(&s.heap, path)
	s.mu.Unlock()
}

func (s *schedulerImpl) RemovePath(path scheduler.SchedulablePath) {
	fmt.Println("remove", path.String())

	cPath, ok := path.(*cfsPath)
	if !ok {
		panic("invalid path underlying type")
	}
	prom.NodeConnInPool.Delete(prometheus.Labels{"addr": path.String()})

	s.mu.Lock()
	defer s.mu.Unlock()

	if cPath.heapIdx < 0 {
		panic("path has been removed")
	}
	heap.Remove(&s.heap, cPath.heapIdx)
}

func (s *schedulerImpl) findBestPath(size int) (*cfsPath, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.heap.Len() == 0 {
		return nil, scheduler.ErrNoPath
	}
	bestPath := s.heap[0]
	bestPath.beforeWrite(size)
	heap.Fix(&s.heap, bestPath.heapIdx)
	return bestPath, nil
}

func (s *schedulerImpl) Write(b *mempool.Buffer) (err error) {
	path, err := s.findBestPath(b.Len())
	if err != nil {
		return
	}
	return path.Write(b)
}
