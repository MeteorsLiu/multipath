package prober

import (
	"context"
	"fmt"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

type Manager struct {
	mu    sync.RWMutex
	inMap map[string]*Prober
}

func NewManager() *Manager {
	return &Manager{inMap: make(map[string]*Prober)}
}

func (i *Manager) Register(ctx context.Context, on func(Event)) (id uuid.UUID, prober *Prober) {
	id = uuid.New()
	prober = New(ctx, on)

	i.mu.Lock()
	i.inMap[id.String()] = prober
	i.mu.Unlock()

	return
}

func (i *Manager) Get(proberId string) *Prober {
	i.mu.RLock()
	prober := i.inMap[proberId]
	i.mu.RUnlock()

	return prober
}

func (i *Manager) Remove(proberId string) {
	i.mu.Lock()
	delete(i.inMap, proberId)
	i.mu.Unlock()
}

func (i *Manager) PacketIn(pkt *mempool.Buffer) {
	id := pkt.Peek(16)
	proberId, err := uuid.FromBytes(id)
	if err != nil {
		return
	}
	proberIdString := proberId.String()
	if prober := i.Get(proberIdString); prober != nil {
		prober.In() <- pkt
		prober.Start(proberId)
	}
	fmt.Println("packet in", i.inMap, proberIdString)
}
