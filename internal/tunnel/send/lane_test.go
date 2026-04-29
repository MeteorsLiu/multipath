package send

import (
	"context"
	"net"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestLaneRuntimeLegQualitiesUDP(t *testing.T) {
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

	udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
	if !udpQ.Active {
		t.Fatal("UDP leg should be active")
	}
	if !tcpQ.Active {
		t.Fatal("TCP leg should be active")
	}
	if charge := legCharge(udpLeg, len("payload")); charge != uint32(len("payload")) {
		t.Fatalf("charge = %d, want %d", charge, len("payload"))
	}
	if udpLeg.Kind != transport.KindUDP || udpLeg.EndpointID != "udp0" {
		t.Fatalf("udpLeg = %+v, want UDP udp0", udpLeg)
	}
	if udpLeg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("remote = %s, want 127.0.0.1:1234", udpLeg.RemoteAddr)
	}
	if tcpLeg.Kind != transport.KindTCP || tcpLeg.ConnID != "tcp0" {
		t.Fatalf("tcpLeg = %+v, want TCP tcp0", tcpLeg)
	}
}

func TestLaneRuntimeLegQualitiesOnlyTCP(t *testing.T) {
	lane := newLaneRuntime(1, 10)
	lane.observeLeg(transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	})

	_, udpQ, tcpLeg, tcpQ := lane.legQualities()
	if udpQ.Active {
		t.Fatal("UDP leg should not be active")
	}
	if !tcpQ.Active {
		t.Fatal("TCP leg should be active")
	}
	if charge := legCharge(tcpLeg, len("payload")); charge != uint32(len("payload")+2) {
		t.Fatalf("charge = %d, want %d", charge, len("payload")+2)
	}
	if tcpLeg.Kind != transport.KindTCP || tcpLeg.ConnID != "tcp0" {
		t.Fatalf("tcpLeg = %+v, want TCP tcp0", tcpLeg)
	}
}

func TestLaneRuntimeLegQualitiesUnavailable(t *testing.T) {
	lane := newLaneRuntime(1, 10)
	_, udpQ, _, tcpQ := lane.legQualities()
	if udpQ.Active || tcpQ.Active {
		t.Fatal("Neither leg should be active for unavailable lane")
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
