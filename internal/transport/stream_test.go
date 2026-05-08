package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func TestStreamLengthPrefixedReadWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp: %v", err)
	}
	defer listener.Close()

	server := NewStream(listener)
	client := NewStream(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverEvents := make(chan Payload, 4)
	clientEvents := make(chan Payload, 4)
	go func() { _ = server.Run(ctx, eventChanWriter{events: serverEvents}) }()
	go func() { _ = client.Run(ctx, eventChanWriter{events: clientEvents}) }()

	clientRef, err := client.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if _, err := client.Write(ctx, clientRef.ConnID, []byte("one")); err != nil {
		t.Fatalf("client Write one: %v", err)
	}
	if _, err := client.Write(ctx, clientRef.ConnID, []byte("two")); err != nil {
		t.Fatalf("client Write two: %v", err)
	}

	first := readEvent(t, serverEvents)
	second := readEvent(t, serverEvents)
	if string(first.Packet.Payload) != "one" {
		t.Fatalf("first payload = %q, want one", first.Packet.Payload)
	}
	if string(second.Packet.Payload) != "two" {
		t.Fatalf("second payload = %q, want two", second.Packet.Payload)
	}

	if _, err := server.Write(ctx, first.Leg.ConnID, []byte("ack")); err != nil {
		t.Fatalf("server Write ack: %v", err)
	}
	first.Packet.Release()
	second.Packet.Release()

	ack := readEvent(t, clientEvents)
	if string(ack.Packet.Payload) != "ack" {
		t.Fatalf("ack payload = %q, want ack", ack.Packet.Payload)
	}
	ack.Packet.Release()
}

func TestStreamDialReadLoopOutlivesDialContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp: %v", err)
	}
	defer listener.Close()

	server := NewStream(listener)
	client := NewStream(nil)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	serverEvents := make(chan Payload, 4)
	clientEvents := make(chan Payload, 4)
	go func() { _ = server.Run(runCtx, eventChanWriter{events: serverEvents}) }()
	go func() { _ = client.Run(runCtx, eventChanWriter{events: clientEvents}) }()

	dialCtx, cancelDial := context.WithCancel(runCtx)
	clientRef, err := client.Dial(dialCtx, listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	cancelDial()

	if _, err := client.Write(runCtx, clientRef.ConnID, []byte("hello")); err != nil {
		t.Fatalf("client Write hello: %v", err)
	}
	hello := readEvent(t, serverEvents)
	if string(hello.Packet.Payload) != "hello" {
		t.Fatalf("hello payload = %q, want hello", hello.Packet.Payload)
	}
	hello.Packet.Release()

	time.Sleep(150 * time.Millisecond)
	if _, err := server.Write(runCtx, hello.Leg.ConnID, []byte("ack")); err != nil {
		t.Fatalf("server Write ack: %v", err)
	}

	ack := readEvent(t, clientEvents)
	if string(ack.Packet.Payload) != "ack" {
		t.Fatalf("ack payload = %q, want ack", ack.Packet.Payload)
	}
	ack.Packet.Release()
}

func TestStreamRejectsOversizedFrame(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	stream := NewStream(nil)
	connID := stream.addConn(client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	oversized := make([]byte, maxStreamFrameLen)
	if _, err := stream.Write(ctx, connID, oversized); err != ErrFrameTooLarge {
		t.Fatalf("Write oversized err = %v, want ErrFrameTooLarge", err)
	}
}

func TestStreamWriteUnknownConnNotifiesFailure(t *testing.T) {
	stream := NewStream(nil)
	failures := make(chan LegRef, 1)
	stream.SetFailureHandler(legFailureHandlerFunc(func(ctx context.Context, leg LegRef, err error) {
		failures <- leg
	}))

	if _, err := stream.Write(context.Background(), "missing", []byte("payload")); err != ErrUnknownConn {
		t.Fatalf("Write err = %v, want ErrUnknownConn", err)
	}
	select {
	case leg := <-failures:
		if leg.Kind != KindTCP || leg.ConnID != "missing" {
			t.Fatalf("failure leg = %+v, want tcp missing", leg)
		}
	case <-time.After(time.Second):
		t.Fatal("missing failure callback")
	}
}

func TestStreamReadLoopExitNotifiesFailure(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	stream := NewStream(nil)
	connID := stream.addConn(client)
	failures := make(chan LegRef, 1)
	stream.SetFailureHandler(legFailureHandlerFunc(func(ctx context.Context, leg LegRef, err error) {
		failures <- leg
	}))

	go stream.readLoop(context.Background(), connID, client, eventChanWriter{events: make(chan Payload, 1)})
	_ = server.Close()

	select {
	case leg := <-failures:
		if leg.Kind != KindTCP || leg.ConnID != connID {
			t.Fatalf("failure leg = %+v, want tcp %s", leg, connID)
		}
	case <-time.After(time.Second):
		t.Fatal("missing failure callback")
	}
}

func TestStreamWriteCompletesPartialConnWrites(t *testing.T) {
	conn := &partialWriteConn{maxChunk: 2}
	stream := NewStream(nil)
	connID := stream.addConn(conn)

	n, err := stream.Write(context.Background(), connID, []byte("payload"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len("payload") {
		t.Fatalf("Write n = %d, want %d", n, len("payload"))
	}

	got := conn.buf.Bytes()
	if len(got) != 2+len("payload") {
		t.Fatalf("written frame len = %d, want %d", len(got), 2+len("payload"))
	}
	if frameLen := binary.BigEndian.Uint16(got[:2]); frameLen != uint16(len("payload")) {
		t.Fatalf("frame len = %d, want %d", frameLen, len("payload"))
	}
	if !bytes.Equal(got[2:], []byte("payload")) {
		t.Fatalf("payload = %q, want payload", got[2:])
	}
}

func TestStreamWriteErrorClosesConn(t *testing.T) {
	conn := &errorWriteConn{err: errors.New("write failed")}
	stream := NewStream(nil)
	connID := stream.addConn(conn)

	if _, err := stream.Write(context.Background(), connID, []byte("payload")); err == nil {
		t.Fatal("Write err = nil, want error")
	}
	if !conn.closed {
		t.Fatal("conn was not closed after write error")
	}
}

type partialWriteConn struct {
	net.Conn
	buf      bytes.Buffer
	maxChunk int
}

type errorWriteConn struct {
	net.Conn
	err    error
	closed bool
}

type legFailureHandlerFunc func(context.Context, LegRef, error)

func (f legFailureHandlerFunc) OnLegFailure(ctx context.Context, leg LegRef, err error) {
	f(ctx, leg, err)
}

func (c *partialWriteConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *partialWriteConn) Write(payload []byte) (int, error) {
	n := len(payload)
	if c.maxChunk > 0 && n > c.maxChunk {
		n = c.maxChunk
	}
	return c.buf.Write(payload[:n])
}

func (c *errorWriteConn) Write(payload []byte) (int, error) {
	return 0, c.err
}

func (c *errorWriteConn) Close() error {
	c.closed = true
	return nil
}
