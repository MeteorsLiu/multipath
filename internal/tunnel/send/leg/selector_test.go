package leg

import (
	"testing"
	"time"
)

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

func TestQualitySelectorFallsBackToTCPWhenUDPBandwidthQoS(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, BandwidthBps: 20_000_000, ProbeLoss: 0.10, BandwidthQoSLimited: true, BandwidthPreferTCP: true}
	tcp := Quality{Active: true, DeliveryRate: 1.0, BandwidthBps: 120_000_000, ProbeSamples: 3}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (UDP bandwidth QoS)")
	}
}

func TestQualitySelectorKeepsUDPWhenQoSConfidenceLow(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, BandwidthBps: 20_000_000, ProbeLoss: 0.10, BandwidthQoSLimited: true, BandwidthPreferTCP: false}
	tcp := Quality{Active: true, DeliveryRate: 1.0, BandwidthBps: 120_000_000, ProbeSamples: 3}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP when TCP is not preferred")
	}
}

func TestQualitySelectorBalancesPassiveBytesBeforeBandwidthReady(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, PassiveBytes: 10_000}
	tcp := Quality{Active: true, DeliveryRate: 1.0, PassiveBytes: 3_000}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP while TCP has fewer passive bytes")
	}
}

func TestQualitySelectorKeepsUDPWhenPassiveBytesTied(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, PassiveBytes: 10_000}
	tcp := Quality{Active: true, DeliveryRate: 1.0, PassiveBytes: 10_000}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP on passive byte tie")
	}
}

func TestQualitySelectorFallsBackToTCPWhenPassiveBandwidthMuchHigher(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, PassiveBandwidthBps: 10_000_000, PassiveSamples: 10}
	tcp := Quality{Active: true, DeliveryRate: 1.0, PassiveBandwidthBps: 16_000_000, PassiveSamples: 10}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP when passive TCP bandwidth is much higher")
	}
}

func TestQualitySelectorKeepsUDPWhenPassiveBandwidthClose(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, DeliveryRate: 1.0, PassiveBandwidthBps: 10_000_000, PassiveSamples: 10}
	tcp := Quality{Active: true, DeliveryRate: 1.0, PassiveBandwidthBps: 14_000_000, PassiveSamples: 10}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP when passive bandwidth is close")
	}
}

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

func TestQualitySelectorNoneActive(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("Pick should return false when neither active")
	}
}

func TestUDPPrefersSelectorPrefersUDP(t *testing.T) {
	sel := UDPPrefersSelector{}
	udp := Quality{Active: true, DeliveryRate: 0.10}
	tcp := Quality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("UDPPrefersSelector should always pick UDP when active")
	}
}

func TestUDPPrefersSelectorFallsBackToTCP(t *testing.T) {
	sel := UDPPrefersSelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: true, DeliveryRate: 1.0}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatal("UDPPrefersSelector should fall back to TCP")
	}
}

func TestUDPPrefersSelectorNoneActive(t *testing.T) {
	sel := UDPPrefersSelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: false}

	if _, ok := sel.Pick(udp, tcp); ok {
		t.Fatal("UDPPrefersSelector should return false when neither active")
	}
}
