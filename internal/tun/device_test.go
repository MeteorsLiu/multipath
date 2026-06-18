package tun

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
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

func TestRunWriterDrainsQueuedPacketsIntoBatchSink(t *testing.T) {
	sink := &memoryBatchSink{batchSize: 4}
	packets := make(chan *packetbuf.Packet, 3)
	packets <- testPacket("one")
	packets <- testPacket("two")
	packets <- testPacket("three")
	close(packets)

	if err := RunWriter(context.Background(), packets, sink); err != nil {
		t.Fatalf("RunWriter failed: %v", err)
	}
	if len(sink.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(sink.batches))
	}
	got := sink.batches[0]
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("batch size = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("batch[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func testPacket(value string) *packetbuf.Packet {
	packet := packetbuf.Acquire(len(value))
	copy(packet.Payload, value)
	packet.SetLen(len(value))
	return packet
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

type memoryBatchSink struct {
	batchSize int
	batches   [][]string
}

func (m *memoryBatchSink) WritePacket(context.Context, []byte) (int, error) {
	panic("WritePacket should not be used when batch sink is available")
}

func (m *memoryBatchSink) WritePackets(_ context.Context, packets []*packetbuf.Packet) (int, error) {
	batch := make([]string, 0, len(packets))
	for _, packet := range packets {
		if packet != nil {
			batch = append(batch, string(packet.Payload))
		}
	}
	m.batches = append(m.batches, batch)
	return len(batch), nil
}

func (m *memoryBatchSink) BatchSize() int {
	return m.batchSize
}
