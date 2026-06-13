package runtime

import (
	"context"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

type QoSWriter struct {
	mu      sync.RWMutex
	send    *send.Send
	enabled map[qosKey]struct{}
}

func NewQoSWriter(s *send.Send) *QoSWriter {
	return &QoSWriter{
		send:    s,
		enabled: make(map[qosKey]struct{}),
	}
}

type qosKey struct {
	sessionID uint64
	laneID    uint8
}

func (w *QoSWriter) Enable(sessionID uint64, laneID uint8) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.enabled[qosKey{sessionID: sessionID, laneID: laneID}] = struct{}{}
	w.mu.Unlock()
}

func (w *QoSWriter) Write(ctx context.Context, status recv.QoSStatus) error {
	if w == nil || w.send == nil {
		return nil
	}
	w.mu.RLock()
	_, enabled := w.enabled[qosKey{sessionID: status.SessionID, laneID: status.LaneID}]
	w.mu.RUnlock()
	if !enabled {
		return nil
	}
	legKind, ok := protocolLegKind(status.Kind)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return w.send.WriteFrame(ctx, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeLinkStatus,
		SessionID: status.SessionID,
		LaneID:    status.LaneID,
		Body: protocol.LinkStatusBody{
			LegKind:      legKind,
			Reason:       status.Reason,
			DeliveredBps: status.DeliveredBps,
		},
	}, transport.LegRef{})
}

func protocolLegKind(kind transport.Kind) (uint8, bool) {
	switch kind {
	case transport.KindUDP:
		return protocol.LinkStatusLegUDP, true
	case transport.KindTCP:
		return protocol.LinkStatusLegTCP, true
	default:
		return 0, false
	}
}
