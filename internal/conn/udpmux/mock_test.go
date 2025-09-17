package udpmux

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func generateBuf(size int) []byte {
	buf := make([]byte, size)

	io.ReadFull(rand.Reader, buf)

	return buf
}

func newIPPacket() []byte {
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
	return buf.Bytes()

}

func TestReceiver(t *testing.T) {
	conn, err := net.ListenPacket("udp", "0.0.0.0:9999")
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()

	ch := make(chan *mempool.Buffer, 1024)
	reader := newUDPReceiver(conn, ch, nil, func(s string) {
		fmt.Println("bbb", s)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		reader.readLoop()
	}()

	wg.Wait()

	local, err := net.Dial("udp", "127.0.0.1:9999")
	if err != nil {
		t.Error(err)
		return
	}
	defer local.Close()

	pkt1 := newIPPacket()
	pkt2 := newIPPacket()

	var header [1]byte
	protocol.MakeHeader(header[:], protocol.TunEncap)

	local.Write(append(header[:], pkt1[0:50]...))
	local.Write(pkt1[50:58])
	local.Write(pkt1[58:])

	local.Write(append(header[:], pkt2...))
	buf1 := <-ch
	if !bytes.Equal(buf1.Bytes(), pkt1) {
		t.Errorf("unexpected packet: got: %v, want: %v", buf1.Bytes(), pkt1)
	}
	buf2 := <-ch
	if !bytes.Equal(buf2.Bytes(), pkt2) {
		t.Errorf("unexpected packet: got: %v, want: %v", buf2.Bytes(), pkt2)
	}

}
