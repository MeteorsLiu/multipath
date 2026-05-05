package send

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestQualityLegSelectorPrefersUDPWhenBothHealthy(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 50 * time.Millisecond}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP (both healthy)")
	}
}

func TestQualityLegSelectorFallsBackToTCPWhenUDPDegraded(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}
	tcp := LegQuality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP degraded)")
	}
}

func TestQualityLegSelectorFallsBackToTCPWhenUDPJitterHigh(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 50 * time.Millisecond, RTTVariance: 60 * time.Millisecond}
	tcp := LegQuality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP jitter exceeds mean)")
	}
}

func TestQualityLegSelectorStaysOnUDPWhenTCPAlsoDegraded(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 0.70, SmoothedRTT: 100 * time.Millisecond}
	tcp := LegQuality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP when TCP is also degraded")
	}
}

func TestQualityLegSelectorFallsBackToTCPWhenUDPBandwidthQoS(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 1.0, BandwidthBps: 20_000_000, ProbeLoss: 0.10, BandwidthQoSLimited: true, BandwidthPreferTCP: true}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0, BandwidthBps: 120_000_000, ProbeSamples: 3}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP bandwidth QoS)")
	}
}

func TestQualityLegSelectorKeepsUDPWhenQoSConfidenceLow(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 1.0, BandwidthBps: 20_000_000, ProbeLoss: 0.10, BandwidthQoSLimited: true, BandwidthPreferTCP: false}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0, BandwidthBps: 120_000_000, ProbeSamples: 3}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP when TCP is not preferred")
	}
}

func TestQualityLegSelectorOnlyUDP(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 1.0}
	tcp := LegQuality{Active: false}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("Pick should return UDP")
	}
}

func TestQualityLegSelectorOnlyTCP(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: false}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("Pick should return TCP")
	}
}

func TestQualityLegSelectorNoneActive(t *testing.T) {
	sel := QualityLegSelector{}
	udp := LegQuality{Active: false}
	tcp := LegQuality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("Pick should return false when neither active")
	}
}

func TestUDPPreferssSelectorPrefersUDP(t *testing.T) {
	sel := UDPPreferssSelector{}
	udp := LegQuality{Active: true, DeliveryRate: 0.10}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("UDPPreferssSelector should always pick UDP when active")
	}
}

func TestUDPPreferssSelectorFallsBackToTCP(t *testing.T) {
	sel := UDPPreferssSelector{}
	udp := LegQuality{Active: false}
	tcp := LegQuality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("UDPPreferssSelector should fall back to TCP")
	}
}

func TestUDPPreferssSelectorNoneActive(t *testing.T) {
	sel := UDPPreferssSelector{}
	udp := LegQuality{Active: false}
	tcp := LegQuality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("UDPPreferssSelector should return false when neither active")
	}
}

func TestLegChargeUDP(t *testing.T) {
	charge := legCharge(transport.LegRef{Kind: transport.KindUDP}, 100)
	if charge != 100 {
		t.Fatalf("charge = %d, want 100", charge)
	}
}

func TestLegChargeTCP(t *testing.T) {
	charge := legCharge(transport.LegRef{Kind: transport.KindTCP}, 100)
	if charge != 102 {
		t.Fatalf("charge = %d, want 102", charge)
	}
}
