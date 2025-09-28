package main

import (
	"sync"

	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

type schedulablePathManager struct {
	mu      sync.RWMutex
	pathMap map[string]scheduler.SchedulablePath
}

func newSchedulablePathManager() *schedulablePathManager {
	return &schedulablePathManager{pathMap: make(map[string]scheduler.SchedulablePath)}
}

func (spm *schedulablePathManager) add(addr string, path scheduler.SchedulablePath) {
	spm.mu.Lock()
	spm.pathMap[addr] = path
	spm.mu.Unlock()
}

func (spm *schedulablePathManager) get(addr string) scheduler.SchedulablePath {
	spm.mu.RLock()
	path := spm.pathMap[addr]
	spm.mu.RUnlock()
	return path
}

func (spm *schedulablePathManager) getOrSet(addr string, path scheduler.SchedulablePath) (scheduler.SchedulablePath, bool) {
	spm.mu.Lock()
	defer spm.mu.Unlock()

	if path, ok := spm.pathMap[addr]; ok {
		return path, true
	}
	spm.pathMap[addr] = path
	return path, false
}

func (spm *schedulablePathManager) remove(addr string) {
	spm.mu.Lock()
	delete(spm.pathMap, addr)
	spm.mu.Unlock()
}
