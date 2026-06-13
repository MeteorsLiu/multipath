package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestQoSWindowStaysSilentBelowSampleFloor(t *testing.T) {
	w := newQoSWindow(qosConfig{
		Window:      3 * time.Second,
		Sustain:     3 * time.Second,
		SampleFloor: 100,
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	for i := uint32(0); i < 20; i++ {
		w.ObserveData(transport.KindUDP, i, 1200, time.Unix(0, int64(i)))
	}
	if got := w.Evaluate(time.Unix(3, 0)); len(got) != 0 {
		t.Fatalf("statuses = %+v, want none below sample floor", got)
	}
}

func TestQoSWindowDetectsLimitedDataLegFromRepairProgress(t *testing.T) {
	now := time.Unix(0, 0)
	w := newQoSWindow(qosConfig{
		Window:      3 * time.Second,
		Sustain:     3 * time.Second,
		SampleFloor: 100,
		Now:         func() time.Time { return now },
	})
	for group := uint32(0); group < 40; group++ {
		base := group * 4
		w.ObserveRepair(transport.KindTCP, base, 4, 1200, now.Add(time.Duration(group)*time.Millisecond))
		if group%4 == 0 {
			w.ObserveData(transport.KindUDP, base, 1200, now.Add(time.Duration(group)*time.Millisecond))
		}
	}
	if got := w.Evaluate(now.Add(3 * time.Second)); len(got) != 0 {
		t.Fatalf("first evaluate = %+v, want sustain pending", got)
	}
	got := w.Evaluate(now.Add(6 * time.Second))
	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one limited status", got)
	}
	if got[0].LegKind != protocol.LinkStatusLegUDP || got[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
	if got[0].DeliveredBps == 0 {
		t.Fatal("DeliveredBps is zero")
	}
}

func TestQoSWindowDetectsBackloggedDataLegFromLag(t *testing.T) {
	now := time.Unix(0, 0)
	w := newQoSWindow(qosConfig{
		Window:      3 * time.Second,
		Sustain:     3 * time.Second,
		SampleFloor: 100,
		LagSlack:    300 * time.Millisecond,
		Now:         func() time.Time { return now },
	})
	for group := uint32(0); group < 40; group++ {
		base := group * 4
		repairAt := now.Add(time.Duration(group) * time.Millisecond)
		w.ObserveRepair(transport.KindUDP, base, 4, 1200, repairAt)
		for offset := uint32(0); offset < 4; offset++ {
			w.ObserveData(transport.KindTCP, base+offset, 1200, repairAt.Add(900*time.Millisecond))
		}
	}
	_ = w.Evaluate(now.Add(3 * time.Second))
	got := w.Evaluate(now.Add(6 * time.Second))
	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one backlogged status", got)
	}
	if got[0].LegKind != protocol.LinkStatusLegTCP || got[0].Reason != protocol.LinkStatusReasonBacklogged {
		t.Fatalf("status = %+v, want TCP backlogged", got[0])
	}
}
