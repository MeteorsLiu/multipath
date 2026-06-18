package tun

import (
	"context"
	"errors"
	"io"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const defaultMTU = 1500

type Device struct {
	rw      io.ReadWriteCloser
	mtu     int
	name    string
	pending []*packetbuf.Packet
}

type batchReader interface {
	ReadPackets([]*packetbuf.Packet) (int, error)
	BatchSize() int
}

type batchWriter interface {
	WritePackets([]*packetbuf.Packet) (int, error)
	BatchSize() int
}

func NewDevice(rw io.ReadWriteCloser, mtu int) *Device {
	return newDevice(rw, mtu, "")
}

func newDevice(rw io.ReadWriteCloser, mtu int, name string) *Device {
	if mtu <= 0 {
		mtu = defaultMTU
	}
	return &Device{
		rw:   rw,
		mtu:  mtu,
		name: name,
	}
}

func (d *Device) Name() string {
	return d.name
}

func (d *Device) ReadPacket(ctx context.Context) (*packetbuf.Packet, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if len(d.pending) > 0 {
		packet := d.pending[0]
		copy(d.pending, d.pending[1:])
		d.pending[len(d.pending)-1] = nil
		d.pending = d.pending[:len(d.pending)-1]
		return packet, nil
	}

	if reader, ok := d.rw.(batchReader); ok && reader.BatchSize() > 1 {
		return d.readBatch(ctx, reader)
	}

	packet := packetbuf.Acquire(d.mtu)
	n, err := d.rw.Read(packet.Payload)
	if err != nil {
		packet.Release()
		return nil, err
	}
	packet.SetLen(n)
	return packet, nil
}

func (d *Device) readBatch(ctx context.Context, reader batchReader) (*packetbuf.Packet, error) {
	batchSize := reader.BatchSize()
	if batchSize < 1 {
		batchSize = 1
	}
	packets := make([]*packetbuf.Packet, batchSize)
	packets[0] = packetbuf.Acquire(d.mtu)

	n, err := reader.ReadPackets(packets)
	if n > len(packets) {
		n = len(packets)
	}
	if err != nil && (n == 0 || !errors.Is(err, ErrTooManySegments)) {
		for _, packet := range packets {
			if packet != nil {
				packet.Release()
			}
		}
		return nil, err
	}
	if n == 0 {
		for _, packet := range packets {
			if packet != nil {
				packet.Release()
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		return nil, io.ErrNoProgress
	}
	for i := n; i < len(packets); i++ {
		if packets[i] != nil {
			packets[i].Release()
			packets[i] = nil
		}
	}
	if n > 1 {
		d.pending = append(d.pending, packets[1:n]...)
	}
	return packets[0], nil
}

func (d *Device) WritePacket(ctx context.Context, packet []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}

	return d.rw.Write(packet)
}

func (d *Device) WritePackets(ctx context.Context, packets []*packetbuf.Packet) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}

	if writer, ok := d.rw.(batchWriter); ok && writer.BatchSize() > 1 {
		return writer.WritePackets(packets)
	}
	written := 0
	for _, packet := range packets {
		if packet == nil {
			continue
		}
		if _, err := d.rw.Write(packet.Payload); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func (d *Device) BatchSize() int {
	if writer, ok := d.rw.(batchWriter); ok {
		if n := writer.BatchSize(); n > 0 {
			return n
		}
	}
	if reader, ok := d.rw.(batchReader); ok {
		if n := reader.BatchSize(); n > 0 {
			return n
		}
	}
	return 1
}

func (d *Device) Close() error {
	return d.rw.Close()
}
