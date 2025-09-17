package prober

import (
	"context"
	"fmt"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

var ErrProberIDNotFound = fmt.Errorf("proberid not found")

type Manager struct {
	mu    sync.RWMutex
	inMap map[string]*Prober
}

func NewManager() *Manager {
	return &Manager{inMap: make(map[string]*Prober)}
}

func (i *Manager) Register(ctx context.Context, addr string, on ProberCallback) (id uuid.UUID, prober *Prober) {
	id = uuid.New()
	prober = New(ctx, addr, on)

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

func (i *Manager) PacketIn(ctx context.Context, pkt *mempool.Buffer) error {
	epoch, id, err := ProberIDFromBuffer(pkt)
	if err != nil {
		return err
	}
	proberId := id.String()

	incrEpoch(epoch, pkt)

	i.mu.RLock()
	prober, ok := i.inMap[proberId]
	i.mu.RUnlock()

	fmt.Println(ok, proberId, i.inMap)
	if !ok {
		return ErrProberIDNotFound
	}
	select {
	case prober.In() <- pkt:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}
