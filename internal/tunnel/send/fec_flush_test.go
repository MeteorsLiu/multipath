package send

import (
	"testing"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestComputeFECFlushMsUsesRTTMultiplier(t *testing.T) {
	in := New(Config{
		FECFlushAlpha: 2,
		FECFlushMinMs: 1,
		FECFlushMaxMs: 100,
	})
	lane := newLaneRuntime(1, 1)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	lane.rttUDP.Add(20)
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = lane

	if got := in.computeFECFlushMs(99); got != 40 {
		t.Fatalf("computeFECFlushMs = %d, want 40", got)
	}
}

func TestComputeFECFlushMsUsesColdStartAsRTTInput(t *testing.T) {
	in := New(Config{
		FECFlushAlpha:       2,
		FECFlushMinMs:       1,
		FECFlushMaxMs:       100,
		FECFlushColdStartMs: 20,
	})

	if got := in.computeFECFlushMs(99); got != 40 {
		t.Fatalf("computeFECFlushMs = %d, want 40", got)
	}
}

func TestComputeFECFlushMsClampsAndFixedOverride(t *testing.T) {
	in := New(Config{
		FECFlushAlpha:       2,
		FECFlushMinMs:       2,
		FECFlushMaxMs:       30,
		FECFlushColdStartMs: 20,
	})
	if got := in.computeFECFlushMs(99); got != 30 {
		t.Fatalf("computeFECFlushMs = %d, want 30", got)
	}

	in = New(Config{
		FECFlushFixedMs: 7,
	})
	if got := in.computeFECFlushMs(99); got != 7 {
		t.Fatalf("computeFECFlushMs fixed = %d, want 7", got)
	}
}
