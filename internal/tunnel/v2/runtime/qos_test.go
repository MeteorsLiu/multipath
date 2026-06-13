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

func TestQoSWriterWritesLinkStatusFrame(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		SessionManager: sessions,
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
	sessionID, hello := waitForRuntimeHello(t, s, 1)
	handler := NewRecvHandler(s, sessions)
	if err := handler.OnHelloAck(ctx, transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}}, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      hello.Nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC | protocol.CapLinkStatus,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}); err != nil {
		t.Fatalf("OnHelloAck: %v", err)
	}

	writer := NewQoSWriter(s)
	writer.Enable(sessionID, 1)

	err := writer.Write(ctx, recv.QoSStatus{
		SessionID:    sessionID,
		LaneID:       1,
		Kind:         transport.KindUDP,
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: 2_000_000,
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
			body, ok := frame.Body.(protocol.LinkStatusBody)
			if !ok {
				t.Fatalf("body type = %T, want LinkStatusBody", frame.Body)
			}
			if body.LegKind != protocol.LinkStatusLegUDP || body.Reason != protocol.LinkStatusReasonLimited || body.DeliveredBps != 2_000_000 {
				t.Fatalf("body = %+v, want UDP limited 2000000", body)
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
		SessionID:    1,
		LaneID:       1,
		Kind:         transport.KindUDP,
		Reason:       protocol.LinkStatusReasonLimited,
		DeliveredBps: 2_000_000,
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
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && frame.Type == protocol.TypeHELLO && frame.LaneID == laneID {
				return frame.SessionID, frame.Body.(protocol.HelloBody)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out waiting for HELLO")
	return 0, protocol.HelloBody{}
}
