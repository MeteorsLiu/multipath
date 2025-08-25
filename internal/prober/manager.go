package prober

import (
	"context"
	"fmt"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
)

type Manager struct {
	mu    sync.RWMutex
	inMap map[string]*Prober
}

func NewManager() *Manager {
	return &Manager{inMap: make(map[string]*Prober)}
}

func (i *Manager) Register(remoteAddr string, ctx context.Context, on func(Event)) (prober *Prober, ok bool) {
	i.mu.Lock()
	_, ok = i.inMap[remoteAddr]
	if !ok {
		prober = New(ctx, remoteAddr, on)
		i.inMap[remoteAddr] = prober
	}
	i.mu.Unlock()

	return
}

func (i *Manager) Get(remoteAddr string) *Prober {
	i.mu.RLock()
	prober := i.inMap[remoteAddr]
	i.mu.RUnlock()

	return prober
}

func (i *Manager) Remove(remoteAddr string) {
	i.mu.Lock()
	delete(i.inMap, remoteAddr)
	i.mu.Unlock()
}

func (i *Manager) PacketIn(remoteAddr string, pkt *mempool.Buffer) {
	if prober := i.Get(remoteAddr); prober != nil {
		prober.In() <- pkt
	}
	fmt.Println("packet in", i.inMap, remoteAddr)
}
