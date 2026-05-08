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
		block: make(chan struct{}),
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

type runWriterStream struct {
	err    error
	block  chan struct{}
	writes int
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
