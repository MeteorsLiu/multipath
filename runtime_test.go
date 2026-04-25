package main

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/probe"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

func TestRuntimeReturnsContextWhenNoLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := (&appRuntime{}).Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
}

func TestRuntimeReturnsFirstLoopError(t *testing.T) {
	boom := errors.New("runtime boom")
	packet := &runtimePacketTransport{
		err:     boom,
		started: make(chan struct{}),
	}

	err := (&appRuntime{packetTransport: packet}).Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	assertClosed(t, packet.started, "packet transport start")
}

func TestRuntimeCancelsOtherLoopsAfterError(t *testing.T) {
	boom := errors.New("runtime boom")
	packet := &runtimePacketTransport{
		err:     boom,
		started: make(chan struct{}),
	}
	stream := &runtimeStreamTransport{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}

	err := (&appRuntime{
		packetTransport: packet,
		streamTransport: stream,
	}).Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want %v", err, boom)
	}
	assertClosed(t, packet.started, "packet transport start")
	assertClosed(t, stream.started, "stream transport start")
	assertClosed(t, stream.canceled, "stream transport cancel")
}

func TestRuntimeBootstrapFailureDoesNotStartLoops(t *testing.T) {
	sender := send.New(send.Config{
		BootstrapLanes: []send.BootstrapLane{
			{
				SessionID: 1,
				LaneID:    protocol.SessionControlLaneID,
				Weight:    1,
			},
		},
	})
	packet := &runtimePacketTransport{started: make(chan struct{})}

	err := (&appRuntime{
		send:            sender,
		probeLoop:       probe.New(sender),
		packetTransport: packet,
	}).Run(context.Background())
	if err == nil {
		t.Fatal("Run err = nil, want bootstrap error")
	}
	select {
	case <-packet.started:
		t.Fatal("packet transport started after bootstrap failure")
	default:
	}
}

type runtimePacketTransport struct {
	err     error
	started chan struct{}
}

func (p *runtimePacketTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	if p.started != nil {
		close(p.started)
	}
	if p.err != nil {
		return p.err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (p *runtimePacketTransport) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	return len(payload), nil
}

type runtimeStreamTransport struct {
	started  chan struct{}
	canceled chan struct{}
}

func (s *runtimeStreamTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	if s.started != nil {
		close(s.started)
	}
	<-ctx.Done()
	if s.canceled != nil {
		close(s.canceled)
	}
	return ctx.Err()
}

func (s *runtimeStreamTransport) Dial(ctx context.Context, remote string) (transport.LegRef, error) {
	return transport.LegRef{Kind: transport.KindTCP, ConnID: "runtime-test"}, nil
}

func (s *runtimeStreamTransport) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	return len(payload), nil
}

func (s *runtimeStreamTransport) Close(ctx context.Context, connID string) error {
	return nil
}

func assertClosed(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatalf("%s channel is not closed", name)
	}
}
