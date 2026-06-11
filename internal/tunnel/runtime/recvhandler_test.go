package runtime_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/runtime"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

// TestRecvHandlerRoutesControlFramesToSend verifies the runtime handler routes
// decoded control frames into Send's state transitions: a HELLO creates the
// lane and produces a HELLO_ACK, and a following PING produces a PONG, both on
// the observed transport.
func TestRecvHandlerRoutesControlFramesToSend(t *testing.T) {
	in := send.New()
	handler := runtime.NewRecvHandler(in)

	// The handler must satisfy recv.Handler (this is also how app.go wires it).
	var _ recv.Handler = handler

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
	}

	if err := handler.OnHello(context.Background(), leg, protocol.Frame{
		Type:      protocol.TypeHELLO,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.HelloBody{Nonce: 1, Caps: protocol.CapTCPFallback},
	}); err != nil {
		t.Fatalf("OnHello failed: %v", err)
	}
	ackFrame := readFrame(t, in)
	if ackFrame.Type != protocol.TypeHELLOACK {
		t.Fatalf("first reply = type %d, want HELLO_ACK", ackFrame.Type)
	}

	if err := handler.OnPing(context.Background(), leg, protocol.Frame{
		Type:      protocol.TypePING,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.PingBody{PingID: 7, TimeMS: 12345},
	}); err != nil {
		t.Fatalf("OnPing failed: %v", err)
	}
	pongFrame := readFrame(t, in)
	if pongFrame.Type != protocol.TypePONG {
		t.Fatalf("ping reply = type %d, want PONG", pongFrame.Type)
	}
	body, ok := pongFrame.Body.(protocol.PingBody)
	if !ok || body.PingID != 7 || body.TimeMS != 12345 {
		t.Fatalf("PONG body = %+v, want pingID=7 timeMS=12345", pongFrame.Body)
	}
}

func readFrame(t *testing.T, in *send.Send) protocol.Frame {
	t.Helper()
	select {
	case payload := <-in.Packets():
		defer payload.Packet.Release()
		frame, err := protocol.Decode(payload.Packet.Payload)
		if err != nil {
			t.Fatalf("Decode reply: %v", err)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send payload")
		return protocol.Frame{}
	}
}
