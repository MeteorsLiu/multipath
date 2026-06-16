package send

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
)

// TestLegDeliveryRateDrivesSelector verifies the #1 fix end to end: once the
// ping delivery signal is fed into the leg observer, a UDP path with high loss
// and a healthy TCP path makes selectRef switch DATA from UDP to TCP — the
// loss-shaped QoS (selector rule 3) that was dead before delivery was wired.
func TestLegDeliveryRateDrivesSelector(t *testing.T) {
	g := newLeg(transport.KindUDP, &selector.QualitySelector{})
	g.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"})
	g.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "t"})
	g.markActive(transport.KindUDP)
	g.markActive(transport.KindTCP)

	// Both healthy initially → selector default = UDP carries DATA.
	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindUDP {
		t.Fatalf("initial primary = %v, want UDP", ref.Kind)
	}

	// Feed delivery samples (need >= deliveryMinSamples=8 to leave the optimistic
	// 1.0 default): UDP loses most, TCP delivers all.
	for i := 0; i < 16; i++ {
		// UDP: ~25% on-time → delivery rate well under minUDPDelivery (0.80).
		g.observeDelivery(transport.KindUDP, i%4 == 0)
		// TCP: always on-time → >= minTCPDelivery (0.90).
		g.observeDelivery(transport.KindTCP, true)
	}

	// Now loss-shaped QoS must move DATA to TCP and REPAIR to UDP.
	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindTCP {
		t.Fatalf("primary after UDP loss = %v, want TCP (loss-shaped QoS)", ref.Kind)
	}
	if ref := g.selectRef(roleShadow); ref.Kind != transport.KindUDP {
		t.Fatalf("shadow after UDP loss = %v, want UDP", ref.Kind)
	}
}

// TestLegDeliveryHealthyStaysUDP verifies the inverse: when both paths deliver
// well, the selector keeps DATA on UDP (the fix must not spuriously switch).
func TestLegDeliveryHealthyStaysUDP(t *testing.T) {
	g := newLeg(transport.KindUDP, &selector.QualitySelector{})
	g.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"})
	g.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "t"})
	g.markActive(transport.KindUDP)
	g.markActive(transport.KindTCP)

	for i := 0; i < 16; i++ {
		g.observeDelivery(transport.KindUDP, true)
		g.observeDelivery(transport.KindTCP, true)
	}

	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindUDP {
		t.Fatalf("primary with both healthy = %v, want UDP", ref.Kind)
	}
}
