package transport

import (
	"context"
	"net"
	"testing"
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

type discardConn struct {
	net.Conn
}

func (discardConn) Write(payload []byte) (int, error) {
	return len(payload), nil
}
