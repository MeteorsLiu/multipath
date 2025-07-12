package scheduler

import (
	"container/heap"
	"errors"
	"sync"
)

var ErrNoPath = errors.New("failed to get path: no available path")

type Scheduler struct {
	mu      sync.Mutex
	heap    pathHeap
	pathMap map[string]*Path
}

func NewScheduler() *Scheduler {
	return &Scheduler{pathMap: make(map[string]*Path)}
}

func (s *Scheduler) AddPath(path *Path) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.pathMap[path.Addr()]; ok {
		return
	}
	heap.Push(&s.heap, path)
	s.pathMap[path.Addr()] = path

	path.setOnPathUp(func() {
		s.AddPath(path)
	})
	path.setOnPathDown(func() {
		s.RemovePath(path)
	})
	path.setOnLost(func() {
		s.RemovePath(path)
	})
}

func (s *Scheduler) RemovePath(path *Path) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pathInMap, ok := s.pathMap[path.Addr()]
	if !ok {
		return
	}
	heap.Remove(&s.heap, pathInMap.heapIdx)
	delete(s.pathMap, pathInMap.Addr())
}

func (s *Scheduler) Write(b []byte) (n int, err error) {
	path, err := s.findBestPath(b)
	if err != nil {
		return
	}
	return path.Conn().Write(b)
}

func (s *Scheduler) findBestPath(b []byte) (*Path, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.heap.Len() == 0 {
		return nil, ErrNoPath
	}
	bestPath := s.heap[len(s.heap)-1]
	bestPath.prepareWrite(b)
	heap.Fix(&s.heap, bestPath.heapIdx)
	return bestPath, nil
}
