package conn

import (
	"sync"
)

type connElem struct {
	writer   ConnWriter
	onRemove func()
}

type SenderManager struct {
	mu          sync.RWMutex
	connMap     map[string]*connElem
	onConnEvent func(ManagerEvent, ConnWriter)
}

type Options func(*SenderManager)

func WithOnNewPath(fn func(ManagerEvent, ConnWriter)) Options {
	return func(pm *SenderManager) {
		pm.onConnEvent = fn
	}
}

func NewManager(opts ...Options) *SenderManager {
	pm := &SenderManager{connMap: make(map[string]*connElem)}

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

func (pm *SenderManager) Get(addr string) ConnWriter {
	pm.mu.RLock()
	sender := pm.connMap[addr]
	pm.mu.RUnlock()

	return sender.writer
}

func (pm *SenderManager) Add(addr string, fn func() (w ConnWriter, onRemove func())) (conn ConnWriter, succ bool) {
	var onRemove func()
	pm.mu.Lock()
	if _, ok := pm.connMap[addr]; ok {
		pm.mu.Unlock()
		return
	}
	if conn, onRemove = fn(); conn != nil {
		succ = true
		pm.connMap[addr] = &connElem{conn, onRemove}
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
	elem, ok := pm.connMap[conn.String()]
	delete(pm.connMap, conn.String())
	pm.mu.Unlock()

	if ok {
		pm.onRemovePath(conn)
		if elem.onRemove != nil {
			go elem.onRemove()
		}
	}
	return ok
}
