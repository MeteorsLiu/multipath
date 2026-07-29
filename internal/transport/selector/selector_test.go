package selector

import "testing"

// Default: both active and no QoS/PreferTCP → UDP.
func TestQualitySelectorPrefersUDPWhenBothHealthy(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true}
	tcp := Quality{Active: true}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if !useUDP {
		t.Fatal("Pick returned TCP, want UDP (both healthy)")
	}
}

// PreferTCP cold-start lock: quality normal but PreferTCP set → TCP.
func TestQualitySelectorHonorsPreferTCP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, PreferTCP: true}
	tcp := Quality{Active: true}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok {
		t.Fatal("Pick returned false, want true")
	}
	if useUDP {
		t.Fatal("Pick returned UDP, want TCP (PreferTCP set)")
	}
}

func TestQualitySelectorQoSPrefersHealthyLeg(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 2_000_000}
	tcp := Quality{Active: true}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatalf("Pick = %v/%v, want TCP", useUDP, ok)
	}
}

func TestQualitySelectorQoSChoosesHigherBpsWhenBothBad(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 2_000_000}
	tcp := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 8_000_000}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || useUDP {
		t.Fatalf("Pick = %v/%v, want TCP with higher delivered bps", useUDP, ok)
	}
}

func TestQualitySelectorQoSStatusSwitchesImmediately(t *testing.T) {
	sel := QualitySelector{}
	udpBad := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 2_000_000}
	tcpGood := Quality{Active: true}
	if useUDP, ok := sel.Pick(udpBad, tcpGood); !ok || useUDP {
		t.Fatalf("first Pick = %v/%v, want TCP", useUDP, ok)
	}

	udpGood := Quality{Active: true}
	tcpBad := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 1_000_000}
	if useUDP, ok := sel.Pick(udpGood, tcpBad); !ok || !useUDP {
		t.Fatalf("Pick after opposite QoS status = %v/%v, want UDP", useUDP, ok)
	}
}

func TestQualitySelectorQoSClearReturnsToUDPImmediately(t *testing.T) {
	sel := QualitySelector{}
	udpBad := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 2_000_000}
	udpGood := Quality{Active: true}
	tcpGood := Quality{Active: true}

	if useUDP, ok := sel.Pick(udpBad, tcpGood); !ok || useUDP {
		t.Fatalf("first Pick = %v/%v, want TCP", useUDP, ok)
	}
	if useUDP, ok := sel.Pick(udpGood, tcpGood); !ok || !useUDP {
		t.Fatalf("Pick after clear = %v/%v, want UDP", useUDP, ok)
	}
}

func TestQualitySelectorQoSNewLimitedStatusOverridesClearImmediately(t *testing.T) {
	sel := QualitySelector{}
	udpBad := Quality{Active: true, QoSActive: true, QoSDeliveredBps: 2_000_000}
	udpGood := Quality{Active: true}
	tcpGood := Quality{Active: true}
	if useUDP, ok := sel.Pick(udpBad, tcpGood); !ok || useUDP {
		t.Fatalf("first Pick = %v/%v, want TCP", useUDP, ok)
	}
	if useUDP, ok := sel.Pick(udpGood, tcpGood); !ok || !useUDP {
		t.Fatalf("Pick after clear = %v/%v, want UDP", useUDP, ok)
	}
	if useUDP, ok := sel.Pick(udpBad, tcpGood); !ok || useUDP {
		t.Fatalf("Pick after new UDP limited status = %v/%v, want TCP", useUDP, ok)
	}
}

// Rule 2: only one transport active → that one.
func TestQualitySelectorOnlyUDP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: true}
	tcp := Quality{Active: false}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("Pick should return UDP")
	}
}

func TestQualitySelectorOnlyTCP(t *testing.T) {
	sel := QualitySelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: true}

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
	udp := Quality{Active: true}
	tcp := Quality{Active: true}

	useUDP, ok := sel.Pick(udp, tcp)
	if !ok || !useUDP {
		t.Fatal("UDPPreferSelector should always pick UDP when active")
	}
}

func TestUDPPreferSelectorFallsBackToTCP(t *testing.T) {
	sel := UDPPreferSelector{}
	udp := Quality{Active: false}
	tcp := Quality{Active: true}

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
