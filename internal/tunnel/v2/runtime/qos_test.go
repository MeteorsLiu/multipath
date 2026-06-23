package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

type qosFakeStreamTransport struct{}

func (qosFakeStreamTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (qosFakeStreamTransport) Dial(ctx context.Context, remote string) (transport.LegRef, error) {
	return transport.LegRef{Kind: transport.KindTCP, ConnID: "qos-tcp"}, nil
}

func (qosFakeStreamTransport) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	return len(payload), nil
}

func (qosFakeStreamTransport) Close(ctx context.Context, connID string) error {
	return nil
}

func TestQoSWriterWritesLinkStatusFrame(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		StreamTransport: qosFakeStreamTransport{},
		SessionManager:  sessions,
		BootstrapLanes: []send.BootstrapLane{{
			LaneID:    1,
			Weight:    100,
			Leg:       transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}},
			TCPRemote: "qos-tcp",
		}},
		ProbeInterval: time.Hour,
		ProbeTimeout:  time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	handler := NewRecvHandler(s, sessions)
	sessionID, udpHello, udpLeg := waitForRuntimeHelloOnKind(t, s, 1, transport.KindUDP)
	if err := handler.OnHelloAck(ctx, udpLeg, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      udpHello.Nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC | protocol.CapLinkStatus,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}); err != nil {
		t.Fatalf("OnHelloAck UDP: %v", err)
	}
	_, tcpHello, tcpLeg := waitForRuntimeHelloOnKind(t, s, 1, transport.KindTCP)
	if err := handler.OnHelloAck(ctx, tcpLeg, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      tcpHello.Nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC | protocol.CapLinkStatus,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}); err != nil {
		t.Fatalf("OnHelloAck TCP: %v", err)
	}

	writer := NewQoSWriter(s)
	writer.Enable(sessionID, 1)

	err := writer.Write(ctx, recv.QoSStatus{
		SessionID:       sessionID,
		LaneID:          1,
		UDPLimited:      true,
		RepairCount:     3,
		UDPDeliveredBps: 2_000_000,
		TCPDeliveredBps: 8_000_000,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err != nil {
				continue
			}
			if frame.Type != protocol.TypeLinkStatus || frame.SessionID != sessionID || frame.LaneID != 1 {
				continue
			}
			if payload.Leg.Kind != transport.KindTCP {
				t.Fatalf("LINK_STATUS leg = %+v, want TCP", payload.Leg)
			}
			body, ok := frame.Body.(protocol.LinkStatusBody)
			if !ok {
				t.Fatalf("body type = %T, want LinkStatusBody", frame.Body)
			}
			if body.Status != 0x54 || body.UDPDeliveredBps != 2_000_000 || body.TCPDeliveredBps != 8_000_000 {
				t.Fatalf("body = %+v, want UDP limited snapshot", body)
			}
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("missing LINK_STATUS output")
}

func TestQoSWriterDropsBeforeNegotiation(t *testing.T) {
	s := send.New(send.Config{
		BootstrapLanes: []send.BootstrapLane{{
			LaneID: 1,
			Weight: 100,
			Leg:    transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}},
		}},
		ProbeInterval: time.Hour,
		ProbeTimeout:  time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	_, _ = waitForRuntimeHello(t, s, 1)

	writer := NewQoSWriter(s)
	if err := writer.Write(ctx, recv.QoSStatus{
		SessionID:       1,
		LaneID:          1,
		UDPLimited:      true,
		UDPDeliveredBps: 2_000_000,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && frame.Type == protocol.TypeLinkStatus {
				t.Fatalf("unexpected LINK_STATUS frame = %+v", frame)
			}
		case <-deadline:
			return
		}
	}
}

func waitForRuntimeHello(t *testing.T, s *send.Send, laneID uint8) (uint64, protocol.HelloBody) {
	t.Helper()
	sessionID, body, _ := waitForRuntimeHelloOnKind(t, s, laneID, 0)
	return sessionID, body
}

func waitForRuntimeHelloOnKind(t *testing.T, s *send.Send, laneID uint8, kind transport.Kind) (uint64, protocol.HelloBody, transport.LegRef) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			leg := payload.Leg
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && frame.Type == protocol.TypeHELLO && frame.LaneID == laneID && (kind == 0 || leg.Kind == kind) {
				return frame.SessionID, frame.Body.(protocol.HelloBody), leg
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out waiting for HELLO")
	return 0, protocol.HelloBody{}, transport.LegRef{}
}
