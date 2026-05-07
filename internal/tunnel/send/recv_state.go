package send

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

type RecvState struct {
	sender *Send
}

func NewRecvState(sender *Send) *RecvState {
	return &RecvState{sender: sender}
}

func (s *RecvState) OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=HELLO")
		return protocol.ErrInvalidFrame
	}
	return s.sender.acceptHello(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=HELLO_ACK")
		return protocol.ErrInvalidFrame
	}
	return s.sender.acceptHelloAck(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=PING")
		return protocol.ErrInvalidFrame
	}
	return s.sender.receivePing(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=PONG")
		return protocol.ErrInvalidFrame
	}
	return s.sender.receivePong(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=CLOSE")
		return protocol.ErrInvalidFrame
	}
	return s.sender.close(ctx, frame.SessionID, frame.LaneID, body.Scope, body.Reason)
}

func (s *RecvState) OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.BandwidthProbeBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=BW_PROBE")
		return protocol.ErrInvalidFrame
	}
	return s.sender.receiveBandwidthProbe(ctx, frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.BandwidthProbeAckBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=BW_PROBE_ACK")
		return protocol.ErrInvalidFrame
	}
	return s.sender.receiveBandwidthProbeAck(frame.SessionID, frame.LaneID, leg, body)
}

func (s *RecvState) OnBandwidthProbeDone(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.BandwidthProbeDoneBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=BW_PROBE_DONE")
		return protocol.ErrInvalidFrame
	}
	s.sender.markBandwidthProbeDone(frame.SessionID, frame.LaneID)
	debuglog.Printf("send/control", "bw_probe_done session=%d lane=%d client_bps=%d", frame.SessionID, frame.LaneID, body.ResultBps)
	return nil
}
