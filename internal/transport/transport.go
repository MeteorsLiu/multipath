package transport

import (
	"context"
	"errors"
	"net"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

type Kind uint8

const (
	KindUDP Kind = iota + 1
	KindTCP
)

type LegRef struct {
	Kind Kind

	EndpointID string
	RemoteAddr net.Addr

	ConnID string
}

var ErrInvalidLeg = errors.New("transport: invalid leg")

type Payload struct {
	Leg    LegRef
	Packet *packetbuf.Packet
}

type PacketWriter interface {
	WriteTo(ctx context.Context, leg LegRef, packet *packetbuf.Packet) error
}

type LegFailureHandler interface {
	OnLegFailure(ctx context.Context, leg LegRef, err error)
}

type PacketTransport interface {
	Run(ctx context.Context, writer PacketWriter) error
	WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error)
}

type StreamTransport interface {
	Run(ctx context.Context, writer PacketWriter) error
	Dial(ctx context.Context, remote string) (LegRef, error)
	Write(ctx context.Context, connID string, payload []byte) (int, error)
	Close(ctx context.Context, connID string) error
}

func RunWriter(ctx context.Context, packets <-chan Payload, packet PacketTransport, stream StreamTransport) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case payload, ok := <-packets:
			if !ok {
				return nil
			}
			if payload.Packet == nil {
				continue
			}
			if debuglog.Enabled() {
				debuglog.Printf("transport", "writer dispatch %s bytes=%d", debugLeg(payload.Leg), len(payload.Packet.Payload))
			}
			err := writePayload(ctx, payload, packet, stream)
			payload.Packet.Release()
			if err != nil {
				debuglog.Printf("transport", "writer error %s err=%v", debugLeg(payload.Leg), err)
				metrics.IncCounter(metrics.TransportErrorsTotal,
					metrics.L("transport", kindLabel(payload.Leg.Kind)),
					metrics.L("operation", "write_dispatch"),
				)
				return err
			}
		}
	}
}

func kindLabel(kind Kind) string {
	switch kind {
	case KindUDP:
		return "udp"
	case KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}

func writePayload(ctx context.Context, payload Payload, packet PacketTransport, stream StreamTransport) error {
	switch payload.Leg.Kind {
	case KindUDP:
		if packet == nil || payload.Leg.EndpointID == "" || payload.Leg.RemoteAddr == nil {
			return ErrInvalidLeg
		}
		_, err := packet.WriteTo(ctx, payload.Leg.EndpointID, payload.Leg.RemoteAddr, payload.Packet.Payload)
		return err
	case KindTCP:
		if stream == nil || payload.Leg.ConnID == "" {
			return ErrInvalidLeg
		}
		_, err := stream.Write(ctx, payload.Leg.ConnID, payload.Packet.Payload)
		if err != nil && !errors.Is(err, context.Canceled) {
			if debuglog.Enabled() {
				debuglog.Printf("transport", "drop stale tcp payload conn=%s bytes=%d err=%v", payload.Leg.ConnID, len(payload.Packet.Payload), err)
			}
			return nil
		}
		return err
	default:
		return ErrInvalidLeg
	}
}
