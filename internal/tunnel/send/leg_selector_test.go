package send

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

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
