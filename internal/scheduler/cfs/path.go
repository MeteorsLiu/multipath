package cfs

import (
	"math"

	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

type cfsPath struct {
	path.Path

	heapIdx     int
	sentBytes   uint64
	weight      int
	virtualSent uint64
}

func NewPath(path path.Path) scheduler.SchedulablePath {
	return &cfsPath{Path: path}
}

func (p *cfsPath) SetWeight(weight int) {
	if p.weight == weight || weight < 0 {
		return
	}
	p.weight = weight
	p.updateVirtualSent()
}

func (p *cfsPath) beforeWrite(size int) {
	// assert sche.Lock()
	p.sentBytes += uint64(size)
	p.updateVirtualSent()
}

func (p *cfsPath) markAsLost() {
	// assert sche.Lock()
	p.virtualSent = math.MaxUint64
}

func (p *cfsPath) updateVirtualSent() {
	p.virtualSent = p.sentBytes / max(uint64(p.weight), 1)
}
