package transport

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func BenchmarkStreamWrite(b *testing.B) {
	stream := NewStream(nil)
	connID := stream.addConn(discardConn{})
	payload := make([]byte, 1430)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		if _, err := stream.Write(context.Background(), connID, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamWriteTCPConn(b *testing.B) {
	stream, connID, cleanup := newTCPWriteBenchmarkStream(b)
	defer cleanup()
	payload := make([]byte, maxStreamFrameLen-1)

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stream.Write(context.Background(), connID, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamWritePayloadBatchTCPConn(b *testing.B) {
	for _, tt := range []struct {
		name        string
		batchSize   int
		payloadSize int
	}{
		{name: "mtu_32", batchSize: 32, payloadSize: 1430},
		{name: "large_4", batchSize: 4, payloadSize: maxStreamFrameLen - 1},
	} {
		b.Run(tt.name, func(b *testing.B) {
			benchmarkStreamWritePayloadBatchTCPConn(b, tt.batchSize, tt.payloadSize)
		})
	}
}

func benchmarkStreamWritePayloadBatchTCPConn(b *testing.B, batchSize int, payloadSize int) {
	stream, connID, cleanup := newTCPWriteBenchmarkStream(b)
	defer cleanup()

	b.ReportAllocs()
	b.SetBytes(int64(batchSize * payloadSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payloads := make([]Payload, batchSize)
		for j := range payloads {
			packet := packetbuf.Acquire(payloadSize)
			payloads[j] = Payload{
				Leg:    LegRef{Kind: KindTCP, ConnID: connID},
				Packet: packet,
			}
		}
		if err := stream.writePayloadBatch(context.Background(), connID, payloads); err != nil {
			b.Fatal(err)
		}
	}
}

func newTCPWriteBenchmarkStream(b *testing.B) (*Stream, string, func()) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen tcp: %v", err)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		b.Fatalf("Dial tcp: %v", err)
	}

	server := <-accepted
	if server == nil {
		_ = client.Close()
		_ = listener.Close()
		b.Fatal("accept failed")
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, server)
		close(done)
	}()

	stream := NewStream(nil)
	connID := stream.addConn(client)
	cleanup := func() {
		_ = stream.Close(context.Background(), connID)
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
		<-done
	}
	return stream, connID, cleanup
}

type discardConn struct {
	net.Conn
}

func (discardConn) Write(payload []byte) (int, error) {
	return len(payload), nil
}
