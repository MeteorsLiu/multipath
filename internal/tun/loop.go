package tun

import (
	"context"
	"io"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
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

type PacketBatchSink interface {
	WritePackets(ctx context.Context, packets []*packetbuf.Packet) (int, error)
	BatchSize() int
}

func Run(ctx context.Context, reader PacketReader, writer PacketWriter) error {
	if closer, ok := reader.(io.Closer); ok {
		go func() {
			<-ctx.Done()
			closer.Close()
		}()
	}

	for {
		packet, err := reader.ReadPacket(ctx)
		if err != nil {
			debuglog.Printf("tun", "read err=%v", err)
			metrics.IncCounter(metrics.TUNErrorsTotal, metrics.L("operation", "read"))
			return err
		}
		debuglog.Printf("tun", "read bytes=%d", len(packet.Payload))
		metrics.IncCounter(metrics.TUNPacketsTotal, metrics.L("direction", "read"))
		metrics.AddCounter(metrics.TUNBytesTotal, uint64(len(packet.Payload)), metrics.L("direction", "read"))
		if err := writer.Write(ctx, packet); err != nil {
			debuglog.Printf("tun", "send write err=%v", err)
			metrics.IncCounter(metrics.TUNErrorsTotal, metrics.L("operation", "deliver"))
			return err
		}
	}
}

func RunWriter(ctx context.Context, packets <-chan *packetbuf.Packet, sink PacketSink) error {
	batchSink, _ := sink.(PacketBatchSink)
	batchSize := 1
	if batchSink != nil {
		batchSize = batchSink.BatchSize()
	}
	if batchSize < 1 {
		batchSize = 1
	}
	batch := make([]*packetbuf.Packet, 0, batchSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case packet, ok := <-packets:
			if !ok {
				return nil
			}
			if packet == nil {
				continue
			}
			batch = append(batch[:0], packet)
			closed := false
		drain:
			for len(batch) < cap(batch) {
				select {
				case next, ok := <-packets:
					if !ok {
						closed = true
						break drain
					}
					if next != nil {
						batch = append(batch, next)
					}
				default:
					break drain
				}
			}
			if err := writeTUNBatch(ctx, sink, batchSink, batch); err != nil {
				return err
			}
			if closed {
				return nil
			}
		}
	}
}

func writeTUNBatch(ctx context.Context, sink PacketSink, batchSink PacketBatchSink, packets []*packetbuf.Packet) error {
	if len(packets) == 0 {
		return nil
	}
	lengths := make([]int, len(packets))
	for i, packet := range packets {
		if packet != nil {
			lengths[i] = len(packet.Payload)
			debuglog.Printf("tun", "write bytes=%d", lengths[i])
		}
	}

	var err error
	if batchSink != nil {
		_, err = batchSink.WritePackets(ctx, packets)
	} else {
		for _, packet := range packets {
			if packet == nil {
				continue
			}
			_, err = sink.WritePacket(ctx, packet.Payload)
			if err != nil {
				break
			}
		}
	}
	for i, packet := range packets {
		if packet != nil {
			packet.Release()
		}
		if err == nil && lengths[i] > 0 {
			metrics.IncCounter(metrics.TUNPacketsTotal, metrics.L("direction", "write"))
			metrics.AddCounter(metrics.TUNBytesTotal, uint64(lengths[i]), metrics.L("direction", "write"))
		}
	}
	if err != nil {
		debuglog.Printf("tun", "write err=%v", err)
		metrics.IncCounter(metrics.TUNErrorsTotal, metrics.L("operation", "write"))
		return err
	}
	return nil
}
