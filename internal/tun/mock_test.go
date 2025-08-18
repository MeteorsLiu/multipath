package tun

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn/batch"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func generateBuf(size int) []byte {
	buf := make([]byte, size)

	io.ReadFull(rand.Reader, buf)

	return buf
}

func TestIncomplete(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
		return
	}
	defer r.Close()
	defer w.Close()

	payload := generateBuf(100)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths: true,
	}
	gopacket.SerializeLayers(buf, opts,
		&layers.Ethernet{},
		&layers.IPv4{
			Version: 4,
		},
		&layers.TCP{},
		gopacket.Payload(payload))
	packetData := buf.Bytes()

	w.Write(packetData[:19])

	sd, err := r.SyscallConn()
	if err != nil {
		t.Fatal(err)
		return
	}

	tun := &tunDevice{tunFile: r, fallbackReader: batch.NewReader(sd)}

	go func() {
		t.Log("start to sleep")
		time.Sleep(100 * time.Millisecond)
		t.Log("end to sleep")

		w.Write(packetData[19:])
	}()

	b := make([][]byte, 1)
	b[0] = make([]byte, 140)

	t.Log("start to read")
	num, n, err := tun.ReadBatch(b)
	t.Log("end to read")

	if err != nil {
		t.Fatal(err)
		return
	}

	if n != 140 || num != 1 {
		t.Fatalf("unexpected buf size: want %v got %v", 140, n)
	}

	if !bytes.Equal(b[0], packetData) {
		t.Fatalf("unexpected buf: want %v got %v", packetData, b[0])
	}

}

func TestComplete(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
		return
	}
	defer r.Close()
	defer w.Close()

	payload := generateBuf(100)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths: true,
	}
	gopacket.SerializeLayers(buf, opts,
		&layers.Ethernet{},
		&layers.IPv4{
			Version: 4,
		},
		&layers.TCP{},
		gopacket.Payload(payload))
	packetData := buf.Bytes()

	_, err = w.Write(packetData)
	if err != nil {
		t.Fatal(err)
		return
	}

	_, err = w.Write(packetData)
	if err != nil {
		t.Fatal(err)
		return
	}

	sd, err := r.SyscallConn()
	if err != nil {
		t.Fatal(err)
		return
	}

	tun := &tunDevice{tunFile: r, fallbackReader: batch.NewReader(sd)}

	b := make([][]byte, 2)
	b[0] = make([]byte, 140)
	b[1] = make([]byte, 140)

	t.Log("start to read")
	num, n, err := tun.ReadBatch(b)
	t.Log("end to read")

	if err != nil {
		t.Fatal(err)
		return
	}

	if n != 280 || num != 2 {
		t.Fatalf("unexpected buf size: want %v got %v", 280, n)
	}

	if !bytes.Equal(b[0], packetData) {
		t.Fatalf("unexpected buf: want %v got %v", packetData, b[0])
	}
	if !bytes.Equal(b[1], packetData) {
		t.Fatalf("unexpected buf: want %v got %v", packetData, b[1])
	}
}

func TestMockTun(t *testing.T) {
	tunInt, err := CreateTUN("multipath-veth0", 1500)
	if err != nil {
		t.Error(err)
		return
	}
	defer tunInt.Close()

	execCommand("ip", "link", "set", "multipath-veth0", "up")
	execCommand("ip", "a", "add", "10.168.168.1", "peer", "10.168.168.2", "dev", "multipath-veth0")

	defer execCommand("ip", "link", "del", "multipath-veth0")

	var buf bytes.Buffer
	b := newBufferWriter(buf)
	NewHandler(context.Background(), tunInt, b)

	execCommand("ping", "-c", "1", "10.168.168.2")
	var ip4 layers.IPv4
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &ip4)
	decoded := []gopacket.LayerType{}

	err = parser.DecodeLayers(buf.Bytes(), &decoded)
	if err != nil {
		t.Error(err)
		return
	}

	for _, layerType := range decoded {
		switch layerType {
		case layers.LayerTypeIPv4:
			fmt.Println("    IP4 ", ip4.SrcIP, ip4.DstIP)
		}
	}
}

type bufferWriter struct {
	bytes.Buffer
}

func newBufferWriter(b bytes.Buffer) *bufferWriter {
	return &bufferWriter{Buffer: b}
}

func (b *bufferWriter) Write(buf *mempool.Buffer) error {
	defer mempool.Put(buf)
	_, err := b.Buffer.Write(buf.Bytes())
	return err
}

func execCommand(cmd string, args ...string) {
	current := exec.Command(cmd, args...)
	current.Stdout = os.Stdout
	current.Stderr = os.Stderr
	current.Run()
}
