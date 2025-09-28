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
)

type pathElem struct {
	path     Path
	onRemove func()
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
		pm.connMap[addr] = &pathElem{p, onRemove}
	}
	pm.mu.Unlock()

	if succ {
		pm.onNewPath(p)
	}

	return
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
		pm.onRemovePath(elem.path)
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
