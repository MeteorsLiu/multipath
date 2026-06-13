package send

import (
	"context"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestSendWrite(t *testing.T) {
	s := New()

	// Create and activate a session
	session, err := sessionpkg.New()
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	var sessionID uint64
	session.Do(func(v sessionpkg.View) error {
		sessionID = v.SessionID()
		return nil
	})

	s.activateSession(sessionID)

	// Create sendState
	s.sendStatesMu.Lock()
	s.sendStates[sessionID] = &sendState{}
	s.sendStatesMu.Unlock()

	// Create a lane
	lane := newLaneRuntime(1, 100)
	lane.bindUDP(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	})
	lane.markActive(transport.KindUDP)

	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane
	s.lanesMu.Unlock()

	// Write a packet
	packet := packetbuf.Acquire(100)
	packet.Payload = []byte("test data")

	ctx := context.Background()
	err = s.Write(ctx, packet)
	if err != nil {
		t.Errorf("Write failed: %v", err)
	}

	// Check that a packet was queued
	select {
	case payload := <-s.Packets():
		payload.Packet.Release()
	default:
		t.Error("expected packet in output queue")
	}
}

func TestSendWriteFrame(t *testing.T) {
	s := New()

	session, err := sessionpkg.New()
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	var sessionID uint64
	session.Do(func(v sessionpkg.View) error {
		sessionID = v.SessionID()
		return nil
	})

	// Create a lane
	lane := newLaneRuntime(1, 100)
	lane.bindUDP(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	})
	lane.markActive(transport.KindUDP)

	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane
	s.lanesMu.Unlock()

	// Write a control frame (PING)
	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePING,
		SessionID: sessionID,
		LaneID:    1,
		Body: protocol.PingBody{
			PingID: 123,
			TimeMS: 1000,
		},
	}

	ctx := context.Background()
	err = s.WriteFrame(ctx, frame, transport.LegRef{})
	if err != nil {
		t.Errorf("WriteFrame failed: %v", err)
	}

	// Check that a packet was queued
	select {
	case payload := <-s.Packets():
		payload.Packet.Release()
	default:
		t.Error("expected packet in output queue")
	}
}

func TestSendWriteFrameExplicitTransport(t *testing.T) {
	s := New()

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePING,
		SessionID: 1,
		LaneID:    1,
		Body: protocol.PingBody{
			PingID: 123,
			TimeMS: 1000,
		},
	}

	ctx := context.Background()
	err := s.WriteFrame(ctx, frame, leg)
	if err != nil {
		t.Errorf("WriteFrame with explicit transport failed: %v", err)
	}

	// Check that a packet was queued
	select {
	case payload := <-s.Packets():
		if payload.Leg != leg {
			t.Error("expected packet on specified leg")
		}
		payload.Packet.Release()
	default:
		t.Error("expected packet in output queue")
	}
}

func TestSendNoRunnableLane(t *testing.T) {
	s := New()

	// Activate session with sendState but don't create any lanes
	s.activateSession(1)
	s.sendStatesMu.Lock()
	s.sendStates[1] = &sendState{}
	s.sendStatesMu.Unlock()

	packet := packetbuf.Acquire(100)
	packet.Payload = []byte("test data")

	ctx := context.Background()
	err := s.Write(ctx, packet)
	if err != ErrNoRunnableLane {
		t.Errorf("expected ErrNoRunnableLane, got %v", err)
	}
}

func TestLaneRuntimeReady(t *testing.T) {
	lane := newLaneRuntime(1, 100)

	if lane.ready() {
		t.Error("expected lane to not be ready initially")
	}

	// Binding a UDP ref records the address but must NOT make the lane ready:
	// readiness is driven by leg.active, which only a peer reply flips on
	// (spec 6.2: active默认false, HELLO_ACK/PONG才置true).
	lane.bindUDP(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	})
	if lane.ready() {
		t.Error("expected lane to NOT be ready after bind without active (spec 6.2)")
	}

	// A peer reply (HELLO_ACK/PONG) marks the transport active → ready.
	lane.markActive(transport.KindUDP)
	if !lane.ready() {
		t.Error("expected lane to be ready after markActive(UDP)")
	}

	// Marking it down again removes readiness.
	lane.markDown(transport.KindUDP)
	if lane.ready() {
		t.Error("expected lane to NOT be ready after markDown(UDP)")
	}
}

type testAddr struct {
	addr string
}

func (a *testAddr) Network() string { return "udp" }
func (a *testAddr) String() string  { return a.addr }
