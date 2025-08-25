package conn

import (
	"sync"
)

type SenderManager struct {
	mu          sync.Mutex
	connMap     map[string]ConnWriter
	onConnEvent func(ManagerEvent, ConnWriter)
}

type Options func(*SenderManager)

func WithOnNewPath(fn func(ManagerEvent, ConnWriter)) Options {
	return func(pm *SenderManager) {
		pm.onConnEvent = fn
	}
}

func NewManager(opts ...Options) *SenderManager {
	pm := &SenderManager{connMap: make(map[string]ConnWriter)}

	for _, opt := range opts {
		opt(pm)
	}

	return pm
}

func (pm *SenderManager) onNewPath(conn ConnWriter) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(ConnAppend, conn)
	}
}

func (pm *SenderManager) onRemovePath(conn ConnWriter) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(ConnRemove, conn)
	}
}

func (pm *SenderManager) Add(addr string, fn func() ConnWriter) (conn ConnWriter, succ bool) {
	pm.mu.Lock()
	if _, ok := pm.connMap[addr]; ok {
		pm.mu.Unlock()
		return
	}
	if conn = fn(); conn != nil {
		succ = true
		pm.connMap[addr] = conn
	}
	pm.mu.Unlock()

	if succ {
		pm.onNewPath(conn)
	}

	return
}

func (pm *SenderManager) Remove(conn ConnWriter) bool {
	if conn == nil {
		return false
	}
	pm.mu.Lock()
	_, ok := pm.connMap[conn.String()]
	delete(pm.connMap, conn.String())
	pm.mu.Unlock()

	if ok {
		pm.onRemovePath(conn)
	}
	return ok
}
