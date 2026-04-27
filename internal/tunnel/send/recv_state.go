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
	s.sender.mu.Lock()
	defer s.sender.mu.Unlock()
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
	s.sender.mu.Lock()
	defer s.sender.mu.Unlock()
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
	s.sender.mu.Lock()
	defer s.sender.mu.Unlock()
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
	s.sender.mu.Lock()
	defer s.sender.mu.Unlock()
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
	s.sender.mu.Lock()
	defer s.sender.mu.Unlock()
	return s.sender.close(ctx, frame.SessionID, frame.LaneID, body.Scope)
}
