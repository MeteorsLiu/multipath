package tun

import (
	"context"
	"io"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const defaultMTU = 1500

type Device struct {
	rw   io.ReadWriteCloser
	mtu  int
	name string
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

	packet := packetbuf.Acquire(d.mtu)
	n, err := d.rw.Read(packet.Payload)
	if err != nil {
		packet.Release()
		return nil, err
	}
	packet.SetLen(n)
	return packet, nil
}

func (d *Device) WritePacket(ctx context.Context, packet []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}

	return d.rw.Write(packet)
}

func (d *Device) Close() error {
	return d.rw.Close()
}
