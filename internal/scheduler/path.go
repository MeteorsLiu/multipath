package scheduler

import (
	"context"
	"net"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux"
	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type Path struct {
	heapIdx     int
	addr        string
	sentBytes   uint64
	weight      int
	virtualSent uint64
	done        context.Context
	// 1:1 binding
	conn *udpmux.UDPMuxer
}

func NewPath(ctx context.Context, packetOut chan<- *mempool.Buffer) *Path {
	return &Path{done: ctx}
}

func (p *Path) setOnPathDown(fn func()) {

}

func (p *Path) setOnPathUp(fn func()) {

}

func (p *Path) setOnLost(fn func()) {

}

// server-side only
func (p *Path) setOnNewPath(fn func()) {

}

func (p *Path) Addr() string {
	return p.addr
}

func (p *Path) Conn() net.Conn {
	return p.addr
}

func (p *Path) SetWeight(weight int) {
	if p.weight == weight || weight < 0 {
		return
	}
	p.weight = weight
	p.updateVirtualSent()
}

func (p *Path) prepareWrite(b []byte) {
	// assert sche.Lock()
	p.sentBytes += uint64(len(b))
	p.updateVirtualSent()
}

func (p *Path) updateVirtualSent() {
	p.virtualSent = p.sentBytes / max(uint64(p.weight), 1)
}

type pathHeap []*Path

func (h pathHeap) Len() int           { return len(h) }
func (h pathHeap) Less(i, j int) bool { return h[i].virtualSent < h[j].virtualSent }
func (h pathHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *pathHeap) Push(x any) {
	n := len(*h)
	item := x.(*Path)
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
