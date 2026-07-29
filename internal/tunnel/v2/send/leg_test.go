package send

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
)

func TestLegQoSStatusDrivesSelector(t *testing.T) {
	g := newLeg(transport.KindUDP, &selector.QualitySelector{})
	g.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"})
	g.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "t"})
	g.markActive(transport.KindUDP)
	g.markActive(transport.KindTCP)

	// Both healthy initially → selector default = UDP carries DATA.
	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindUDP {
		t.Fatalf("initial primary = %v, want UDP", ref.Kind)
	}

	g.observeQoSStatus(true, 1_000, false, 0)

	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindTCP {
		t.Fatalf("primary after UDP QoS = %v, want TCP", ref.Kind)
	}
	if ref := g.selectRef(roleShadow); ref.Kind != transport.KindUDP {
		t.Fatalf("shadow after UDP QoS = %v, want UDP", ref.Kind)
	}
}

func TestLegQoSClearStaysUDP(t *testing.T) {
	g := newLeg(transport.KindUDP, &selector.QualitySelector{})
	g.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"})
	g.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "t"})
	g.markActive(transport.KindUDP)
	g.markActive(transport.KindTCP)

	g.observeQoSStatus(false, 0, false, 0)

	if ref := g.selectRef(rolePrimary); ref.Kind != transport.KindUDP {
		t.Fatalf("primary with clear QoS = %v, want UDP", ref.Kind)
	}
}
