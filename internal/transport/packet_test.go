package transport

import (
	"context"
	"net"
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
