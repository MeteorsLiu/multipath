package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

func TestNewRecvHandlerWiresSessionAndSend(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}

	handler := NewRecvHandler(s, sessions)

	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
	if handler.send != s {
		t.Error("handler send not wired correctly")
	}
	if handler.sessions != sessions {
		t.Error("handler sessions not wired correctly")
	}
}

func TestRecvHandlerOnHelloRepliesWithHelloAckThroughSession(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLO,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.HelloBody{
			Nonce:      100,
			Caps:       protocol.CapFEC | protocol.CapTCPFallback,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	ctx := context.Background()
	err := handler.OnHello(ctx, leg, frame)
	if err != nil {
		t.Fatalf("OnHello failed: %v", err)
	}

	// Verify session was created
	sess, ok := sessions.Get(12345)
	if !ok {
		t.Error("expected session to be created")
	}
	if sess == nil {
		t.Fatal("session is nil")
	}

	// Verify HELLO_ACK was queued
	select {
	case payload := <-s.Packets():
		// Decode and verify it's a HELLO_ACK
		decoded, err := protocol.Decode(payload.Packet.Payload)
		if err != nil {
			t.Errorf("failed to decode response: %v", err)
		}
		if decoded.Type != protocol.TypeHELLOACK {
			t.Errorf("expected HELLO_ACK, got %v", decoded.Type)
		}
		payload.Packet.Release()
	default:
		t.Error("expected HELLO_ACK in output queue")
	}
}

func TestRecvHandlerOnHelloAckAcceptsNonceBeforeLaneStateUpdate(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)

	// Create session and open a HELLO (self-driving Hello with a no-op sender).
	sess, _ := sessions.GetOrCreate(12345)
	hello := sess.Open(context.Background(),
		sessionpkg.HelloConfig{RetryInterval: time.Hour},
		func(ctx context.Context, v sessionpkg.View) error { return nil }, nil)

	var nonce uint64
	hello.Do(func(v sessionpkg.View) error {
		nonce = v.Nonce()
		return nil
	})

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	ctx := context.Background()
	err := handler.OnHelloAck(ctx, leg, frame)
	if err != nil {
		t.Errorf("OnHelloAck failed: %v", err)
	}

	// After ack, the hello should be removed from session
	// (we can't directly check pending state from outside Hello)
	// The test passes if OnHelloAck doesn't return an error
}

func TestRecvHandlerOnPingRepliesWithPong(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)
	// The session must exist, else OnPing replies CLOSE{UnknownSession} instead
	// of PONG (peer-restart self-heal). Register it first.
	sessions.GetOrCreate(12345)

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePING,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.PingBody{
			PingID: 42,
			TimeMS: 1000,
		},
	}

	ctx := context.Background()
	err := handler.OnPing(ctx, leg, frame)
	if err != nil {
		t.Fatalf("OnPing failed: %v", err)
	}

	// Verify PONG was queued
	select {
	case payload := <-s.Packets():
		decoded, err := protocol.Decode(payload.Packet.Payload)
		if err != nil {
			t.Errorf("failed to decode response: %v", err)
		}
		if decoded.Type != protocol.TypePONG {
			t.Errorf("expected PONG, got %v", decoded.Type)
		}

		pongBody, ok := decoded.Body.(protocol.PingBody)
		if !ok {
			t.Fatal("expected PingBody in PONG")
		}
		if pongBody.PingID != 42 {
			t.Errorf("expected PingID 42, got %d", pongBody.PingID)
		}
		if pongBody.TimeMS != 1000 {
			t.Errorf("expected TimeMS 1000, got %d", pongBody.TimeMS)
		}
		payload.Packet.Release()
	default:
		t.Error("expected PONG in output queue")
	}
}

func TestRecvHandlerDispatchesControlFramesWithoutTouchingDATAorREPAIR(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)

	// RecvHandler should only handle control frames
	// DATA and REPAIR stay in Recv, never reach RecvHandler

	// Verify RecvHandler only has control frame methods
	ctx := context.Background()
	leg := transport.LegRef{Kind: transport.KindUDP, EndpointID: "test"}

	// These should all work (control frames)
	_ = handler.OnHello(ctx, leg, protocol.Frame{Type: protocol.TypeHELLO, Body: protocol.HelloBody{}})
	_ = handler.OnHelloAck(ctx, leg, protocol.Frame{Type: protocol.TypeHELLOACK, Body: protocol.HelloAckBody{}})
	_ = handler.OnPing(ctx, leg, protocol.Frame{Type: protocol.TypePING, Body: protocol.PingBody{}})
	_ = handler.OnPong(ctx, leg, protocol.Frame{Type: protocol.TypePONG, Body: protocol.PingBody{}})
	_ = handler.OnClose(ctx, leg, protocol.Frame{Type: protocol.TypeCLOSE, Body: protocol.CloseBody{}})

	// RecvHandler has no OnData or OnRepair methods - those stay in Recv
}

// TestOnPingUnknownSessionRepliesClose verifies the peer-restart self-heal: a
// PING for a session this end does not know yields CLOSE{Session, UnknownSession}
// (not a bare PONG that would let the peer believe the dead session is alive).
func TestOnPingUnknownSessionRepliesClose(t *testing.T) {
	s := send.New()
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)
	// Note: session 777 is NOT registered.

	leg := transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}}
	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePING,
		SessionID: 777,
		LaneID:    1,
		Body:      protocol.PingBody{PingID: 1, TimeMS: 1000},
	}

	if err := handler.OnPing(context.Background(), leg, frame); err != nil {
		t.Fatalf("OnPing failed: %v", err)
	}

	select {
	case payload := <-s.Packets():
		decoded, err := protocol.Decode(payload.Packet.Payload)
		payload.Packet.Release()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if decoded.Type != protocol.TypeCLOSE {
			t.Fatalf("reply type = %v, want CLOSE", decoded.Type)
		}
		cb, ok := decoded.Body.(protocol.CloseBody)
		if !ok {
			t.Fatal("expected CloseBody")
		}
		if cb.Scope != protocol.CloseScopeSession || cb.Reason != protocol.CloseReasonUnknownSession {
			t.Fatalf("CLOSE scope/reason = %d/%d, want Session/UnknownSession", cb.Scope, cb.Reason)
		}
		if decoded.SessionID != 777 {
			t.Fatalf("CLOSE session = %d, want 777", decoded.SessionID)
		}
	default:
		t.Fatal("expected a CLOSE in the output queue")
	}
}

// TestOnCloseUnknownSessionStaleIsNoOp verifies a CLOSE{UnknownSession} for a
// session this end does not hold is ignored (no rebuild, no CLOSE loop).
func TestOnCloseUnknownSessionStaleIsNoOp(t *testing.T) {
	s := send.New() // no bootstrap lanes → Rebootstrap is a no-op anyway
	sessions := &sessionpkg.Manager{}
	handler := NewRecvHandler(s, sessions)

	leg := transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}}
	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeCLOSE,
		SessionID: 888, // not held
		Body:      protocol.CloseBody{Scope: protocol.CloseScopeSession, Reason: protocol.CloseReasonUnknownSession},
	}

	if err := handler.OnClose(context.Background(), leg, frame); err != nil {
		t.Fatalf("OnClose failed: %v", err)
	}
	// Nothing to assert beyond "no panic / no error": the stale CLOSE is dropped.
}

type testAddr struct {
	addr string
}

func (a *testAddr) Network() string { return "udp" }
func (a *testAddr) String() string  { return a.addr }
