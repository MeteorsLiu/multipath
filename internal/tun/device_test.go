package tun

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestDeviceReadWritePacket(t *testing.T) {
	rw := &memoryTun{read: bytes.NewBufferString("packet")}
	device := NewDevice(rw, 1500)

	packet, err := device.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket failed: %v", err)
	}
	if string(packet.Payload) != "packet" {
		t.Fatalf("packet = %q, want packet", packet.Payload)
	}
	packet.Release()

	n, err := device.WritePacket(context.Background(), []byte("out"))
	if err != nil {
		t.Fatalf("WritePacket failed: %v", err)
	}
	if n != 3 || rw.write.String() != "out" {
		t.Fatalf("write = n %d payload %q, want 3/out", n, rw.write.String())
	}
}

type memoryTun struct {
	read  *bytes.Buffer
	write bytes.Buffer
}

func (m *memoryTun) Read(p []byte) (int, error) {
	if m.read == nil {
		return 0, io.EOF
	}
	return m.read.Read(p)
}

func (m *memoryTun) Write(p []byte) (int, error) {
	return m.write.Write(p)
}

func (m *memoryTun) Close() error {
	return nil
}
