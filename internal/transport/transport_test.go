package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func TestRunWriterReleasesPacketOnInvalidLeg(t *testing.T) {
	packet := packetbuf.Acquire(16)
	packets := make(chan Payload, 1)
	packets <- Payload{Packet: packet}
	close(packets)

	err := RunWriter(context.Background(), packets, nil, nil)
	if !errors.Is(err, ErrInvalidLeg) {
		t.Fatalf("RunWriter err = %v, want ErrInvalidLeg", err)
	}
	if packet.Payload != nil {
		t.Fatal("packet was not released after write error")
	}
}

func TestRunWriterDropsStaleTCPConn(t *testing.T) {
	for name, writeErr := range map[string]error{
		"unknown": ErrUnknownConn,
		"closed":  net.ErrClosed,
	} {
		t.Run(name, func(t *testing.T) {
			packet := packetbuf.Acquire(16)
			packets := make(chan Payload, 1)
			packets <- Payload{
				Leg:    LegRef{Kind: KindTCP, ConnID: "stale"},
				Packet: packet,
			}
			close(packets)

			stream := &runWriterStream{err: writeErr}
			if err := RunWriter(context.Background(), packets, nil, stream); err != nil {
				t.Fatalf("RunWriter err = %v, want nil", err)
			}
			if stream.writes != 1 {
				t.Fatalf("stream writes = %d, want 1", stream.writes)
			}
			if packet.Payload != nil {
				t.Fatal("packet was not released after stale TCP write")
			}
		})
	}
}

func TestRunWriterDropsTransientUDPWriteError(t *testing.T) {
	packet := packetbuf.Acquire(16)
	packets := make(chan Payload, 1)
	packets <- Payload{
		Leg: LegRef{
			Kind:       KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		},
		Packet: packet,
	}
	close(packets)

	writer := &runWriterPacket{err: syscall.ENETUNREACH}
	if err := RunWriter(context.Background(), packets, writer, nil); err != nil {
		t.Fatalf("RunWriter err = %v, want nil", err)
	}
	if writer.writes != 1 {
		t.Fatalf("packet writes = %d, want 1", writer.writes)
	}
	if packet.Payload != nil {
		t.Fatal("packet was not released after UDP write error")
	}
}

func TestRunWriterDoesNotBlockUDPBehindBlockedTCP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := make(chan Payload, 2)
	stream := &runWriterStream{
		block:   make(chan struct{}),
		started: make(chan struct{}),
	}
	packet := &runWriterPacket{wrote: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWriter(ctx, packets, packet, stream)
	}()

	tcpPacket := packetbuf.Acquire(16)
	udpPacket := packetbuf.Acquire(16)
	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: tcpPacket,
	}
	packets <- Payload{
		Leg: LegRef{
			Kind:       KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		},
		Packet: udpPacket,
	}

	select {
	case <-packet.wrote:
	case <-time.After(time.Second):
		t.Fatal("UDP write blocked behind TCP write")
	}

	cancel()
	close(stream.block)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWriter err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWriter did not stop after context cancellation")
	}
}

func TestRunWriterDropsWhenLegQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := make(chan Payload, legWriterQueueSize+1)
	stream := &runWriterStream{
		block:   make(chan struct{}),
		started: make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWriter(ctx, packets, nil, stream)
	}()

	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: packetbuf.Acquire(16),
	}

	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not receive first packet")
	}

	for i := 0; i < legWriterQueueSize; i++ {
		packets <- Payload{
			Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
			Packet: packetbuf.Acquire(16),
		}
	}

	deadline := time.After(time.Second)
	for len(packets) > 0 {
		select {
		case <-deadline:
			t.Fatalf("RunWriter did not drain initial packets, remaining=%d", len(packets))
		default:
			time.Sleep(time.Millisecond)
		}
	}

	probePacket := packetbuf.Acquire(16)
	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: probePacket,
	}

	deadline = time.After(time.Second)
	for probePacket.Payload != nil {
		select {
		case err := <-errCh:
			t.Fatalf("RunWriter exited after queue pressure: %v", err)
		case <-deadline:
			t.Fatal("packet was not released after full leg queue drop")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	close(stream.block)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWriter err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWriter did not stop after context cancellation")
	}
}

type runWriterStream struct {
	err     error
	block   chan struct{}
	started chan struct{}
	once    sync.Once
	writes  int
}

func (s *runWriterStream) Run(ctx context.Context, writer PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *runWriterStream) Dial(ctx context.Context, remote string) (LegRef, error) {
	return LegRef{}, nil
}

func (s *runWriterStream) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	s.writes++
	s.once.Do(func() {
		if s.started != nil {
			close(s.started)
		}
	})
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, s.err
}

func (s *runWriterStream) Close(ctx context.Context, connID string) error {
	return nil
}

type runWriterPacket struct {
	once   sync.Once
	wrote  chan struct{}
	err    error
	writes int
}

func (p *runWriterPacket) Run(ctx context.Context, writer PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *runWriterPacket) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	p.writes++
	p.once.Do(func() {
		if p.wrote != nil {
			close(p.wrote)
		}
	})
	return len(payload), p.err
}
