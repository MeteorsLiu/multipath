package send

import (
	"context"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func udpRef(ep string) Ref {
	return Ref{
		Kind:       transport.KindUDP,
		EndpointID: ep,
		RemoteAddr: &testAddr{addr: "127.0.0.1:9000"},
	}
}

func tcpRef(conn string) Ref {
	return Ref{
		Kind:   transport.KindTCP,
		ConnID: conn,
	}
}

// bindActive binds a transport ref and marks it active, mirroring the
// post-handshake state (bind records the address; HELLO_ACK/PONG activates).
func bindActive(l *laneRuntime, ref Ref) {
	switch ref.Kind {
	case transport.KindUDP:
		l.bindUDP(ref)
	case transport.KindTCP:
		l.bindTCP(ref)
	}
	l.markActive(ref.Kind)
}

func bindBoth(l *laneRuntime) {
	bindActive(l, udpRef("ep-udp"))
	bindActive(l, tcpRef("conn-tcp"))
}

func TestLanePrimaryShadowBothReady(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindBoth(l)

	// Initial primary role = UDP, so DATA -> UDP, REPAIR -> TCP.
	if got := l.primaryTransport(); got.Kind != transport.KindUDP {
		t.Errorf("primary kind = %v, want UDP", got.Kind)
	}
	if got := l.shadowTransport(); got.Kind != transport.KindTCP {
		t.Errorf("shadow kind = %v, want TCP", got.Kind)
	}
}

func TestLaneSetPrimaryFlipsRoles(t *testing.T) {
	// Under the new leg model (plan 5.2/5.3), when both transports are active
	// the selector decides which carries DATA and which carries REPAIR.
	// QualitySelector rule 6 defaults to UDP when both are healthy, so setPrimary
	// no longer changes routing — it's explicitly a reserved/no-op entry point
	// (spec: "nothing flips it to a non-initial value this round").
	//
	// This test now verifies that selector is the authority, not primaryKind.
	l := newLaneRuntime(1, 100)
	bindBoth(l)

	// With QualitySelector and both healthy, DATA→UDP, REPAIR→TCP regardless
	// of primaryKind. Flip primaryKind and verify routing stays unchanged.
	l.setPrimary(transport.KindTCP)

	primary := l.primaryTransport()
	shadow := l.shadowTransport()

	// Selector still picks UDP for DATA (rule 6 default), TCP for REPAIR.
	if primary.Kind != transport.KindUDP {
		t.Errorf("primary kind = %v, want UDP (selector rule 6, not primaryKind)", primary.Kind)
	}
	if shadow.Kind != transport.KindTCP {
		t.Errorf("shadow kind = %v, want TCP (selector rule 6)", shadow.Kind)
	}
}

func TestLaneQoSOverridesSelector(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindBoth(l)

	laneQoSInput{lane: l}.OnQoS(transport.KindUDP, protocol.LinkStatusReasonLimited, 2_000_000, time.Now())

	if got := l.primaryTransport(); got.Kind != transport.KindTCP {
		t.Fatalf("primary kind = %v, want TCP after UDP QoS", got.Kind)
	}
	if got := l.shadowTransport(); got.Kind != transport.KindUDP {
		t.Fatalf("shadow kind = %v, want UDP after UDP QoS", got.Kind)
	}
}

func TestLaneQoSSelectionWaitsForBandwidthGate(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindBoth(l)

	l.leg.setPreferTCP(true)
	laneQoSInput{lane: l}.OnQoS(transport.KindTCP, protocol.LinkStatusReasonLimited, 2_000_000, time.Now())

	if got := l.primaryTransportWithQoS(false); got.Kind != transport.KindTCP {
		t.Fatalf("primary kind with QoS gated = %v, want TCP from BW PreferTCP", got.Kind)
	}
	if got := l.primaryTransportWithQoS(true); got.Kind != transport.KindUDP {
		t.Fatalf("primary kind with QoS enabled = %v, want UDP from TCP QoS", got.Kind)
	}
}

func TestLaneQoSSeenDisablesBandwidthPreferTCP(t *testing.T) {
	gated := newLaneRuntime(1, 100)
	bindBoth(gated)
	gated.leg.setPreferTCP(true)
	laneQoSInput{lane: gated}.OnQoS(transport.KindUDP, protocol.LinkStatusReasonLimited, 2_000_000, time.Now().Add(-301*time.Second))
	if got := gated.primaryTransportWithQoS(false); got.Kind != transport.KindTCP {
		t.Fatalf("primary kind with QoS gated = %v, want TCP from BW PreferTCP", got.Kind)
	}

	enabled := newLaneRuntime(1, 100)
	bindBoth(enabled)
	enabled.leg.setPreferTCP(true)
	laneQoSInput{lane: enabled}.OnQoS(transport.KindUDP, protocol.LinkStatusReasonLimited, 2_000_000, time.Now().Add(-301*time.Second))
	if got := enabled.primaryTransportWithQoS(true); got.Kind != transport.KindUDP {
		t.Fatalf("primary kind after QoS evidence = %v, want UDP with BW PreferTCP suppressed", got.Kind)
	}
}

func TestLaneSingleLegDegradeUDPOnly(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindActive(l, udpRef("ep-udp")) // only UDP ready

	primary := l.primaryTransport()
	shadow := l.shadowTransport()

	// Both DATA and REPAIR must land on the same (UDP) leg.
	if primary.Kind != transport.KindUDP {
		t.Errorf("primary kind = %v, want UDP", primary.Kind)
	}
	if shadow.Kind != transport.KindUDP {
		t.Errorf("shadow kind = %v, want UDP (degraded)", shadow.Kind)
	}
	if primary.EndpointID != shadow.EndpointID {
		t.Errorf("degraded primary/shadow differ: %q vs %q", primary.EndpointID, shadow.EndpointID)
	}
}

func TestLaneSingleLegDegradeTCPOnly(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindActive(l, tcpRef("conn-tcp")) // only TCP ready

	primary := l.primaryTransport()
	shadow := l.shadowTransport()

	// primary role is UDP but UDP not ready -> degrade to TCP; shadow also TCP.
	if primary.Kind != transport.KindTCP {
		t.Errorf("primary kind = %v, want TCP (degraded)", primary.Kind)
	}
	if shadow.Kind != transport.KindTCP {
		t.Errorf("shadow kind = %v, want TCP", shadow.Kind)
	}
	if primary.ConnID != shadow.ConnID {
		t.Errorf("degraded primary/shadow differ: %q vs %q", primary.ConnID, shadow.ConnID)
	}
}

func TestLaneNoLegReturnsZeroRef(t *testing.T) {
	l := newLaneRuntime(1, 100)

	if got := l.primaryTransport(); got.Kind != 0 {
		t.Errorf("primary kind = %v, want zero", got.Kind)
	}
	if got := l.shadowTransport(); got.Kind != 0 {
		t.Errorf("shadow kind = %v, want zero", got.Kind)
	}
}

func TestLaneSetPrimaryRejectsInvalidKind(t *testing.T) {
	l := newLaneRuntime(1, 100)
	bindBoth(l)

	l.setPrimary(transport.Kind(0)) // invalid: ignored

	// primary role stays UDP.
	if got := l.primaryTransport(); got.Kind != transport.KindUDP {
		t.Errorf("primary kind = %v, want UDP unchanged", got.Kind)
	}
}

// TestRepairUsesShadowLeg verifies that with FEC on, a full 4-DATA group emits
// DATA on the primary leg (UDP) and the REPAIR on the shadow leg (TCP).
func TestRepairUsesShadowLeg(t *testing.T) {
	s := New()
	s.EnableFEC()

	session, err := sessionpkg.New()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	var sessionID uint64
	session.Do(func(v sessionpkg.View) error {
		sessionID = v.SessionID()
		return nil
	})
	s.activateSession(sessionID)
	s.sendStatesMu.Lock()
	s.sendStates[sessionID] = &sendState{}
	s.sendStatesMu.Unlock()
	s.EnableFEC()

	lane := newLaneRuntime(1, 100)
	bindBoth(lane)
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane
	s.lanesMu.Unlock()

	ctx := context.Background()
	for i := 0; i < maxFECSourceSpan; i++ {
		pkt := packetbuf.Acquire(32)
		pkt.Payload = []byte("payload-data-1234")
		if err := s.Write(ctx, pkt); err != nil {
			t.Fatalf("Write %d failed: %v", i, err)
		}
	}

	var dataOnUDP, repairOnTCP int
	for {
		select {
		case payload := <-s.Packets():
			f, derr := protocol.Decode(payload.Packet.Payload)
			if derr == nil {
				switch f.Type {
				case protocol.TypeDATA:
					if payload.Leg.Kind == transport.KindUDP {
						dataOnUDP++
					}
				case protocol.TypeREPAIR:
					if payload.Leg.Kind == transport.KindTCP {
						repairOnTCP++
					}
				}
			}
			payload.Packet.Release()
			continue
		default:
		}
		break
	}

	if dataOnUDP != maxFECSourceSpan {
		t.Errorf("DATA on UDP(primary) = %d, want %d", dataOnUDP, maxFECSourceSpan)
	}
	if repairOnTCP != 1 {
		t.Errorf("REPAIR on TCP(shadow) = %d, want 1", repairOnTCP)
	}
}
