// Package runtime holds the tunnel runtime glue that wires the Send and Recv
// roles together. Its receive handler routes decoded control frames from Recv
// into Send's control-plane state transitions, so Recv never needs to know
// about Send and Send exposes no semantic control-frame methods to Recv.
package runtime

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

// RecvHandler implements recv.Handler by routing each decoded control frame to
// the matching send-side state transition. It is the runtime-glue replacement
// for the former in-package send adapter and holds no state of its own beyond
// the Send it drives.
type RecvHandler struct {
	send *send.Send
}

// NewRecvHandler returns a recv.Handler backed by s.
func NewRecvHandler(s *send.Send) *RecvHandler {
	return &RecvHandler{send: s}
}

var _ recv.Handler = (*RecvHandler)(nil)

func (h *RecvHandler) OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.AcceptHello(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (h *RecvHandler) OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.AcceptHelloAck(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (h *RecvHandler) OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.ReceivePing(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (h *RecvHandler) OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.ReceivePong(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (h *RecvHandler) OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.ReceiveClose(ctx, frame.SessionID, frame.LaneID, body.Scope, body.Reason)
}

func (h *RecvHandler) OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.BandwidthProbeBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.ReceiveBandwidthProbe(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (h *RecvHandler) OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.BandwidthProbeAckBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return h.send.ReceiveBandwidthProbeAck(frame.SessionID, frame.LaneID, leg, body)
}
