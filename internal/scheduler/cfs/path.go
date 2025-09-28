package cfs

import (
	"container/list"
	"math"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

type cfsPath struct {
	addr         string
	mu           sync.Mutex
	connPaths    *list.List
	connPathsMap map[string]*list.Element

	heapIdx     int
	sentBytes   uint64
	weight      int
	virtualSent uint64
}

func NewPath(addr string) scheduler.SchedulablePath {
	return &cfsPath{connPaths: list.New(), addr: addr, connPathsMap: make(map[string]*list.Element)}
}

func (p *cfsPath) SetWeight(weight int) {
	if p.weight == weight || weight < 0 {
		return
	}
	p.weight = weight
	p.updateVirtualSent()
}

func (p *cfsPath) tryWrite(size int) bool {
	// assert sche.Lock()
	p.sentBytes += uint64(size)
	p.updateVirtualSent()
	return true
}

func (p *cfsPath) markAsLost() {
	// assert sche.Lock()
	p.virtualSent = math.MaxUint64
}

func (p *cfsPath) updateVirtualSent() {
	p.virtualSent = p.sentBytes / max(uint64(p.weight), 1)
}

func (p *cfsPath) setVirtualSent(virtualSent uint64) {
	p.virtualSent = virtualSent
	p.sentBytes = p.virtualSent * max(uint64(p.weight), 1)
}

func (p *cfsPath) AddConnPath(connPath path.Path) {
	p.mu.Lock()
	if _, ok := p.connPathsMap[connPath.PathID()]; ok {
		panic("duplicated append")
	}
	node := p.connPaths.PushBack(connPath)
	p.connPathsMap[connPath.PathID()] = node
	p.mu.Unlock()
}

func (p *cfsPath) RemoveConnPath(connPath path.Path) {
	p.mu.Lock()
	listNode := p.connPathsMap[connPath.PathID()]
	if listNode == nil {
		panic("listNode dones't exist")
	}
	p.connPaths.Remove(listNode)
	delete(p.connPathsMap, connPath.PathID())
	p.mu.Unlock()
}

func (p *cfsPath) getWriter(size int) mempool.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()

	head := p.connPaths.Front()
	if head == nil {
		return nil
	}
	p.connPaths.MoveToBack(head)
	p.tryWrite(size)

	return head.Value.(path.Path)
}

func (p *cfsPath) String() string {
	return p.addr
}
