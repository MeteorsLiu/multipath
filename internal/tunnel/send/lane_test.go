package send

import (
	"context"
	"net"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestLaneRuntimePrefersUDP(t *testing.T) {
	lane := newLaneRuntime(1, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	lane.observeLeg(transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	})

	leg, ok := lane.selectLeg()
	if !ok {
		t.Fatal("selectLeg failed")
	}
	if charge := legCharge(leg, len("payload")); charge != uint32(len("payload")) {
		t.Fatalf("charge = %d, want %d", charge, len("payload"))
	}
	if leg.Kind != transport.KindUDP || leg.EndpointID != "udp0" {
		t.Fatalf("leg = %+v, want UDP udp0", leg)
	}
	if leg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("remote = %s, want 127.0.0.1:1234", leg.RemoteAddr)
	}
}

func TestLaneRuntimeFallsBackToTCP(t *testing.T) {
	lane := newLaneRuntime(1, 10)
	lane.observeLeg(transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	})

	leg, ok := lane.selectLeg()
	if !ok {
		t.Fatal("selectLeg failed")
	}
	if charge := legCharge(leg, len("payload")); charge != uint32(len("payload")+2) {
		t.Fatalf("charge = %d, want %d", charge, len("payload")+2)
	}
	if leg.Kind != transport.KindTCP || leg.ConnID != "tcp0" {
		t.Fatalf("leg = %+v, want TCP tcp0", leg)
	}
}

func TestLaneRuntimeUnavailable(t *testing.T) {
	lane := newLaneRuntime(1, 10)
	if _, ok := lane.selectLeg(); ok {
		t.Fatal("selectLeg succeeded for unavailable lane")
	}
}

type recordPacketWriter struct {
	calls      int
	endpointID string
	remote     net.Addr
	payload    []byte
	wrote      chan struct{}
}

func (w *recordPacketWriter) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	w.calls++
	w.endpointID = endpointID
	w.remote = remote
	w.payload = append([]byte(nil), payload...)
	if w.wrote != nil {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return len(payload), nil
}

type recordStreamWriter struct {
	calls   int
	connID  string
	payload []byte
	wrote   chan struct{}
}

func (w *recordStreamWriter) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	w.calls++
	w.connID = connID
	w.payload = append([]byte(nil), payload...)
	if w.wrote != nil {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return len(payload), nil
}

func mustUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	return udpAddr
}
