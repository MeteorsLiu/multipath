package path

import (
	"sync"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/prom"
	"github.com/prometheus/client_golang/prometheus"
)

type ManagerEvent int

const (
	Nop ManagerEvent = iota
	Append
	Remove
	Suspend
	Resume
)

type pathElem struct {
	path      Path
	onRemove  func()
	suspended bool
}

type PathManager struct {
	mu          sync.RWMutex
	connMap     map[string]*pathElem
	onConnEvent func(ManagerEvent, Path)
}

type Options func(*PathManager)

func WithOnNewPath(fn func(ManagerEvent, Path)) Options {
	return func(pm *PathManager) {
		pm.onConnEvent = fn
	}
}

func NewManager(opts ...Options) *PathManager {
	pm := &PathManager{connMap: make(map[string]*pathElem)}

	for _, opt := range opts {
		opt(pm)
	}

	return pm
}

func (pm *PathManager) onNewPath(conn Path) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(Append, conn)
	}
}

func (pm *PathManager) onRemovePath(conn Path) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(Remove, conn)
	}
}

func (pm *PathManager) onSuspendPath(conn Path) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(Suspend, conn)
	}
}

func (pm *PathManager) onResumePath(conn Path) {
	if pm.onConnEvent != nil {
		pm.onConnEvent(Resume, conn)
	}
}

func (pm *PathManager) Get(addr string) Path {
	pm.mu.RLock()
	sender := pm.connMap[addr]
	pm.mu.RUnlock()

	return sender.path
}

func (pm *PathManager) Add(addr string, fn func() (w conn.ConnWriter, onRemove func())) (p Path, succ bool) {
	pm.mu.Lock()
	if _, ok := pm.connMap[addr]; ok {
		pm.mu.Unlock()
		return
	}
	if cn, onRemove := fn(); cn != nil {
		succ = true
		p = NewPath(cn)
		pm.connMap[addr] = &pathElem{path: p, onRemove: onRemove}
	}
	pm.mu.Unlock()

	if succ {
		pm.onNewPath(p)
	}

	return
}

func (pm *PathManager) Suspend(conn conn.ConnWriter) bool {
	if conn == nil {
		return false
	}
	pm.mu.Lock()
	elem, ok := pm.connMap[conn.String()]
	if !ok || elem.suspended {
		pm.mu.Unlock()
		return false
	}
	elem.suspended = true
	p := elem.path
	pm.mu.Unlock()

	pm.onSuspendPath(p)
	return true
}

func (pm *PathManager) Resume(conn conn.ConnWriter) bool {
	if conn == nil {
		return false
	}
	pm.mu.Lock()
	elem, ok := pm.connMap[conn.String()]
	if !ok || !elem.suspended {
		pm.mu.Unlock()
		return false
	}
	elem.suspended = false
	p := elem.path
	pm.mu.Unlock()

	pm.onResumePath(p)
	return true
}

func (pm *PathManager) Remove(conn conn.ConnWriter) bool {
	if conn == nil {
		return false
	}
	pm.mu.Lock()
	elem, ok := pm.connMap[conn.String()]
	delete(pm.connMap, conn.String())
	pm.mu.Unlock()

	if ok {
		if !elem.suspended {
			pm.onRemovePath(elem.path)
		}
		if elem.onRemove != nil {
			go func() {
				prom.NodeReconnectNum.With(prometheus.Labels{"addr": conn.String()}).Inc()
				elem.onRemove()
				prom.NodeReconnectNum.With(prometheus.Labels{"addr": conn.String()}).Dec()
			}()
		}
	}
	return ok
}
