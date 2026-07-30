package send

import (
	"context"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
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

func TestSelectorEventReason(t *testing.T) {
	tests := []struct {
		name     string
		udp      selector.Quality
		tcp      selector.Quality
		selected transport.Kind
		want     string
	}{
		{
			name:     "qos avoids udp",
			udp:      selector.Quality{Active: true, QoSActive: true, QoSDeliveredBps: 10},
			tcp:      selector.Quality{Active: true, QoSDeliveredBps: 100},
			selected: transport.KindTCP,
			want:     "qos_avoid_udp",
		},
		{
			name:     "prefer tcp",
			udp:      selector.Quality{Active: true, PreferTCP: true},
			tcp:      selector.Quality{Active: true},
			selected: transport.KindTCP,
			want:     "prefer_tcp",
		},
		{
			name:     "only tcp active",
			udp:      selector.Quality{},
			tcp:      selector.Quality{Active: true},
			selected: transport.KindTCP,
			want:     "tcp_only_active",
		},
		{
			name:     "default udp",
			udp:      selector.Quality{Active: true},
			tcp:      selector.Quality{Active: true},
			selected: transport.KindUDP,
			want:     "default_udp",
		},
	}

	for _, tt := range tests {
		if got := selectorEventReason(tt.udp, tt.tcp, tt.selected); got != tt.want {
			t.Fatalf("%s: reason = %q, want %q", tt.name, got, tt.want)
		}
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

func TestSendWriteFrameExplicitTransportKindSelectsBoundLeg(t *testing.T) {
	s := New()
	sessionID := uint64(99)
	lane := newLaneRuntime(1, 100)
	lane.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-lane-1"})

	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane
	s.lanesMu.Unlock()

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

	if err := s.WriteFrame(context.Background(), frame, transport.LegRef{Kind: transport.KindTCP}); err != nil {
		t.Fatalf("WriteFrame with kind-only TCP ref failed: %v", err)
	}

	select {
	case payload := <-s.Packets():
		defer payload.Packet.Release()
		if payload.Leg.Kind != transport.KindTCP || payload.Leg.ConnID != "tcp-lane-1" {
			t.Fatalf("payload leg = %+v, want tcp-lane-1", payload.Leg)
		}
	default:
		t.Fatal("expected packet in output queue")
	}
}

func TestSendWriteDropsWhenNoRunnableLane(t *testing.T) {
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
	if err != nil {
		t.Errorf("expected no error when dropping no-runnable TUN packet, got %v", err)
	}
}

func TestSendWriteDropsWhenSelectedLaneHasNoActiveLeg(t *testing.T) {
	s := New()

	s.activateSession(1)
	s.sendStatesMu.Lock()
	s.sendStates[1] = &sendState{}
	s.sendStatesMu.Unlock()

	lane := newLaneRuntime(1, 100)
	lane.bindUDP(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	})
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: 1, laneID: 1}] = lane
	s.lanesMu.Unlock()

	packet := packetbuf.Acquire(100)
	packet.Payload = []byte("test data")

	if err := s.Write(context.Background(), packet); err != nil {
		t.Errorf("expected no error when dropping no-active-leg TUN packet, got %v", err)
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

func TestLaneScheduleCostChargesAtLeastMTU(t *testing.T) {
	if got := laneScheduleCost(40); got != defaultMTUBytes {
		t.Fatalf("small packet schedule cost = %d, want %d", got, defaultMTUBytes)
	}

	const jumboPayload = defaultMTUBytes + 200
	want := uint32(jumboPayload + 14)
	if got := laneScheduleCost(jumboPayload); got != want {
		t.Fatalf("jumbo packet schedule cost = %d, want %d", got, want)
	}
}

func TestLaneRuntimeCostScalesWithRepairCount(t *testing.T) {
	lane := newLaneRuntime(1, 1)
	if got := lane.Cost(defaultMTUBytes); got != defaultMTUBytes {
		t.Fatalf("default repair cost = %d, want %d", got, defaultMTUBytes)
	}

	lane.setFEC(4)
	if got := lane.Cost(defaultMTUBytes); got != 4*defaultMTUBytes {
		t.Fatalf("max repair cost = %d, want %d", got, 4*defaultMTUBytes)
	}
}

type testAddr struct {
	addr string
}

func (a *testAddr) Network() string { return "udp" }
func (a *testAddr) String() string  { return a.addr }
