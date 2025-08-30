package prober

import (
	"context"
	"fmt"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

type Manager struct {
	mu    sync.Mutex
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

func (i *Manager) Remove(proberId string) {
	i.mu.Lock()
	delete(i.inMap, proberId)
	i.mu.Unlock()
}

func (i *Manager) PacketIn(pkt *mempool.Buffer, proberElem *Prober) error {
	id, err := ProberIDFromBuffer(pkt)
	if err != nil {
		return err
	}
	proberId := id.String()

	var prober *Prober

	i.mu.Lock()
	prober, ok := i.inMap[proberId]
	if !ok {
		prober = proberElem
		i.inMap[proberId] = prober
		prober.Start(id)
	}
	i.mu.Unlock()

	prober.In() <- pkt

	fmt.Println("packet in", i.inMap, proberId)

	return nil
}
