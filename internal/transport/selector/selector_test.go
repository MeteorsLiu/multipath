package selector

import (
	"testing"
	"time"
)

// Rule 6 (default): both healthy → UDP.
func TestQualitySelectorPrefersUDPWhenBothHealthy(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 50 * time.Millisecond}
	tcp := Quality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP (both healthy)")
	}
}

// Rule 3 (loss-form QoS): UDP delivery poor & TCP good → TCP.
func TestQualitySelectorFallsBackToTCPWhenUDPDegraded(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}
	tcp := Quality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP degraded)")
	}
}

// Rule 4 (jitter): UDP RTT variance ≥ mean & TCP good → TCP.
func TestQualitySelectorFallsBackToTCPWhenUDPJitterHigh(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 50 * time.Millisecond, RTTVariance: 60 * time.Millisecond}
	tcp := Quality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP jitter exceeds mean)")
	}
}

// Rule 5 (PreferTCP cold-start lock): quality normal but PreferTCP set → TCP.
func TestQualitySelectorHonorsPreferTCP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 50 * time.Millisecond, PreferTCP: true}
	tcp := Quality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (PreferTCP set)")
	}
}

// Quality signals (rules 3-4) take precedence over PreferTCP (rule 5):
// when UDP is degraded but PreferTCP is false, TCP still wins via the quality rule.
func TestQualitySelectorQualityRuleIndependentOfPreferTCP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond, PreferTCP: false}
	tcp := Quality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("Pick should return TCP via quality rule regardless of PreferTCP")
	}
}

// Rule 6 holds when TCP is also degraded: keep UDP rather than switch to a worse link.
func TestQualitySelectorKeepsUDPWhenTCPAlsoDegraded(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 0.70, SmoothedRTT: 100 * time.Millisecond}
	tcp := Quality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP when TCP is also degraded")
	}
}

// Rule 2: only one transport active → that one.
func TestQualitySelectorOnlyUDP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0}
	tcp := Quality{Active: false}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("Pick should return UDP")
	}
}

func TestQualitySelectorOnlyTCP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("Pick should return TCP")
	}
}

// Rule 1: neither active → ok=false (drop).
func TestQualitySelectorNoneActive(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("Pick should return false when neither active")
	}
}

func TestUDPPreferSelectorPrefersUDP(t *testing.T) {
	sel := UDPPreferSelector{}
	udp := Quality{Active: true, DeliveryRate: 0.10}
	tcp := Quality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("UDPPreferSelector should always pick UDP when active")
	}
}

func TestUDPPreferSelectorFallsBackToTCP(t *testing.T) {
	sel := UDPPreferSelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("UDPPreferSelector should fall back to TCP")
	}
}

func TestUDPPreferSelectorNoneActive(t *testing.T) {
	sel := UDPPreferSelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("UDPPreferSelector should return false when neither active")
	}
}
