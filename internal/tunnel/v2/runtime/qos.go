package runtime

import (
	"context"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
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
		recordLinkStatusEvent("send_drop_not_enabled", status.SessionID, status.LaneID, status.Kind, status.Reason)
		return nil
	}
	legKind, ok := protocolLegKind(status.Kind)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("runtime/qos", "link_status_send session=%d lane=%d kind=%d reason=%d delivered_bps=%d",
		status.SessionID, status.LaneID, status.Kind, status.Reason, status.DeliveredBps)
	err := w.send.WriteFrame(ctx, protocol.Frame{
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
	if err != nil {
		recordLinkStatusEvent("send_error", status.SessionID, status.LaneID, status.Kind, status.Reason)
		eventlog.Printf("link_status", "action=send_error session=%d lane=%d leg=%s reason=%d err=%v",
			status.SessionID, status.LaneID, linkStatusKindLabel(status.Kind), status.Reason, err)
		return err
	}
	recordLinkStatusEvent("send", status.SessionID, status.LaneID, status.Kind, status.Reason)
	return nil
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

func recordLinkStatusEvent(event string, sessionID uint64, laneID uint8, kind transport.Kind, reason uint8) {
	metrics.IncCounter(metrics.LinkStatusEventsTotal,
		metrics.LStr("event", event),
		metrics.LU64("session", sessionID),
		metrics.LU8("lane", laneID),
		metrics.LStr("leg", linkStatusKindLabel(kind)),
		metrics.LU8("reason", reason),
	)
}

func linkStatusKindLabel(kind transport.Kind) string {
	switch kind {
	case transport.KindUDP:
		return "udp"
	case transport.KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}
