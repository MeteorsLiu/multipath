package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func TestPacketSameSocketReadWrite(t *testing.T) {
	leftConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket left: %v", err)
	}
	defer leftConn.Close()

	rightConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket right: %v", err)
	}
	defer rightConn.Close()

	left, err := NewPacket(PacketEndpoint{ID: "left", Conn: leftConn})
	if err != nil {
		t.Fatalf("NewPacket left: %v", err)
	}
	right, err := NewPacket(PacketEndpoint{ID: "right", Conn: rightConn})
	if err != nil {
		t.Fatalf("NewPacket right: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leftEvents := make(chan Payload, 1)
	rightEvents := make(chan Payload, 1)
	go func() { _ = left.Run(ctx, eventChanWriter{events: leftEvents}) }()
	go func() { _ = right.Run(ctx, eventChanWriter{events: rightEvents}) }()

	rightAddr := rightConn.LocalAddr()
	if _, err := left.WriteTo(ctx, "left", rightAddr, []byte("hello")); err != nil {
		t.Fatalf("left WriteTo: %v", err)
	}

	rightPacket := readEvent(t, rightEvents)
	if string(rightPacket.Packet.Payload) != "hello" {
		t.Fatalf("right payload = %q, want hello", rightPacket.Packet.Payload)
	}

	if _, err := right.WriteTo(ctx, rightPacket.Leg.EndpointID, rightPacket.Leg.RemoteAddr, []byte("ack")); err != nil {
		t.Fatalf("right WriteTo observed addr: %v", err)
	}
	rightPacket.Packet.Release()

	leftPacket := readEvent(t, leftEvents)
	if string(leftPacket.Packet.Payload) != "ack" {
		t.Fatalf("left payload = %q, want ack", leftPacket.Packet.Payload)
	}
	if leftPacket.Leg.RemoteAddr.String() != rightConn.LocalAddr().String() {
		t.Fatalf("left remote = %s, want %s", leftPacket.Leg.RemoteAddr, rightConn.LocalAddr())
	}
	leftPacket.Packet.Release()
}

func TestPacketRunClosesConnOnCancel(t *testing.T) {
	conn := newBlockingPacketConn()
	packet, err := NewPacket(PacketEndpoint{ID: "udp", Conn: conn})
	if err != nil {
		t.Fatalf("NewPacket: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- packet.Run(ctx, eventChanWriter{events: make(chan Payload, 1)})
	}()

	select {
	case <-conn.closed:
		t.Fatal("conn closed before context cancellation")
	case <-time.After(150 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Run err = %v, want nil or context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestPacketReadLoopBatchesReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	batch := &packetReadBatchStub{}
	packet := &Packet{bufSize: 16}
	events := make(chan Payload, 3)
	done := make(chan error, 1)
	go func() {
		done <- packet.readLoop(ctx, packetReadEndpoint{
			id:    "udp",
			conn:  newBlockingPacketConn(),
			batch: batch,
		}, eventChanWriter{events: events})
	}()

	for i, want := range []string{"a", "bb", "ccc"} {
		event := readEvent(t, events)
		if got := string(event.Packet.Payload); got != want {
			t.Fatalf("event %d payload = %q, want %q", i, got, want)
		}
		if event.Leg.EndpointID != "udp" {
			t.Fatalf("event %d endpoint = %q, want udp", i, event.Leg.EndpointID)
		}
		if got, want := event.Leg.RemoteAddr.String(), "remote-"+string(rune('0'+i)); got != want {
			t.Fatalf("event %d remote = %q, want %q", i, got, want)
		}
		event.Packet.Release()
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readLoop err = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit after cancellation")
	}
	if batch.calls < 2 {
		t.Fatalf("batch calls = %d, want at least 2", batch.calls)
	}
}

func readEvent(t *testing.T, events <-chan Payload) Payload {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for send packet")
		return Payload{}
	}
}

type eventChanWriter struct {
	events chan<- Payload
}

func (w eventChanWriter) WriteTo(ctx context.Context, leg LegRef, packet *packetbuf.Packet) error {
	event := Payload{Leg: leg, Packet: packet}
	select {
	case w.events <- event:
		return nil
	case <-ctx.Done():
		packet.Release()
		return ctx.Err()
	}
}

type blockingPacketConn struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingPacketConn() *blockingPacketConn {
	return &blockingPacketConn{closed: make(chan struct{})}
}

func (c *blockingPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}

func (c *blockingPacketConn) WriteTo([]byte, net.Addr) (int, error) {
	return 0, errors.New("unexpected WriteTo")
}

func (c *blockingPacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}

func (c *blockingPacketConn) LocalAddr() net.Addr {
	return dummyAddr("udp")
}

func (c *blockingPacketConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

func (c *blockingPacketConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *blockingPacketConn) SetWriteDeadline(time.Time) error {
	return nil
}

type dummyAddr string

func (a dummyAddr) Network() string {
	return string(a)
}

func (a dummyAddr) String() string {
	return string(a)
}

type packetReadBatchStub struct {
	calls int
}

func (b *packetReadBatchStub) batchSize() int {
	return 4
}

func (b *packetReadBatchStub) writeBatchTo(context.Context, net.Addr, []Payload) (int, error) {
	return 0, errors.New("unexpected writeBatchTo")
}

func (b *packetReadBatchStub) readBatchFrom(ctx context.Context, packets []*packetbuf.Packet, remotes []net.Addr) (int, error) {
	b.calls++
	if b.calls > 1 {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	payloads := []string{"a", "bb", "ccc"}
	for i, payload := range payloads {
		copy(packets[i].Payload, payload)
		packets[i].SetLen(len(payload))
		remotes[i] = dummyAddr("remote-" + string(rune('0'+i)))
	}
	return len(payloads), nil
}
