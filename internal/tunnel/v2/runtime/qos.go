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
		recordLinkStatusEvent("send_drop_not_enabled", status.SessionID, status.LaneID, status.UDPLimited, status.TCPLimited)
		return nil
	}
	linkStatus := linkStatusByte(status.UDPLimited, status.TCPLimited)
	debuglog.Printf("runtime/qos", "link_status_send session=%d lane=%d status=%#02x udp_limited=%t udp_delivered_bps=%d tcp_limited=%t tcp_delivered_bps=%d",
		status.SessionID, status.LaneID, linkStatus, status.UDPLimited, status.UDPDeliveredBps, status.TCPLimited, status.TCPDeliveredBps)
	err := w.send.WriteFrame(ctx, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeLinkStatus,
		SessionID: status.SessionID,
		LaneID:    status.LaneID,
		Body: protocol.LinkStatusBody{
			Status:          linkStatus,
			UDPDeliveredBps: status.UDPDeliveredBps,
			TCPDeliveredBps: status.TCPDeliveredBps,
		},
	}, transport.LegRef{Kind: transport.KindTCP})
	if err != nil {
		recordLinkStatusEvent("send_error", status.SessionID, status.LaneID, status.UDPLimited, status.TCPLimited)
		eventlog.Printf("link_status", "action=send_error session=%d lane=%d status=%#02x err=%v",
			status.SessionID, status.LaneID, linkStatus, err)
		return err
	}
	recordLinkStatusEvent("send", status.SessionID, status.LaneID, status.UDPLimited, status.TCPLimited)
	return nil
}

func linkStatusByte(udpLimited, tcpLimited bool) uint8 {
	var status uint8
	if udpLimited {
		status |= protocol.LinkStatusStateLimited << 4
	}
	if tcpLimited {
		status |= protocol.LinkStatusStateLimited
	}
	return status
}

func recordLinkStatusEvent(event string, sessionID uint64, laneID uint8, udpLimited, tcpLimited bool) {
	metrics.IncCounter(metrics.LinkStatusEventsTotal,
		metrics.LStr("event", event),
		metrics.LU64("session", sessionID),
		metrics.LU8("lane", laneID),
		metrics.LStr("udp_limited", boolMetricLabel(udpLimited)),
		metrics.LStr("tcp_limited", boolMetricLabel(tcpLimited)),
	)
}

func boolMetricLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func runtimeKindLabel(kind transport.Kind) string {
	switch kind {
	case transport.KindUDP:
		return "udp"
	case transport.KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}
