package transport

import (
	"context"
	"errors"
	"net"
	"testing"

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

type runWriterStream struct {
	err    error
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
	return 0, s.err
}

func (s *runWriterStream) Close(ctx context.Context, connID string) error {
	return nil
}
