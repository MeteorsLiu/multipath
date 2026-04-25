package tun

import (
	"context"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

type PacketReader interface {
	ReadPacket(ctx context.Context) (*packetbuf.Packet, error)
}

type PacketWriter interface {
	Write(ctx context.Context, packet *packetbuf.Packet) error
}

type PacketSink interface {
	WritePacket(ctx context.Context, packet []byte) (int, error)
}

func Run(ctx context.Context, reader PacketReader, writer PacketWriter) error {
	for {
		packet, err := reader.ReadPacket(ctx)
		if err != nil {
			return err
		}
		if err := writer.Write(ctx, packet); err != nil {
			return err
		}
	}
}

func RunWriter(ctx context.Context, packets <-chan *packetbuf.Packet, sink PacketSink) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case packet, ok := <-packets:
			if !ok {
				return nil
			}
			_, err := sink.WritePacket(ctx, packet.Payload)
			packet.Release()
			if err != nil {
				return err
			}
		}
	}
}
