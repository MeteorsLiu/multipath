package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestLaneArrivalStatsUsesRepairWaterlineForDataExpected(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	if samples := stats.qosSamples(now); len(samples) != 0 {
		t.Fatalf("initial samples = %+v, want none", samples)
	}

	for group := uint32(0); group < 25; group++ {
		stats.recordRepair(transport.KindTCP, group*maxFECSourceSpan, maxFECSourceSpan, 1200, now.Add(3*time.Second))
	}

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing UDP data-leg sample")
	}
	if sample.DataExpected != 100 || sample.DataArrived != 0 {
		t.Fatalf("sample counts = got expected %d arrived %d, want expected=100 arrived=0", sample.DataExpected, sample.DataArrived)
	}
	if sample.RepairKind != transport.KindTCP || sample.RepairBytes != 25*1200 {
		t.Fatalf("repair side = kind %d bytes %d, want TCP %d", sample.RepairKind, sample.RepairBytes, 25*1200)
	}
}

func TestLaneArrivalStatsDoesNotInflateExpectedDataForDuplicateRepair(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordRepair(transport.KindTCP, 0, maxFECSourceSpan, 1200, now.Add(3*time.Second))
	stats.recordRepair(transport.KindTCP, 0, maxFECSourceSpan, 1200, now.Add(3*time.Second))

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing UDP data-leg sample")
	}
	if sample.DataExpected != maxFECSourceSpan {
		t.Fatalf("data expected = %d, want one repair group's span %d", sample.DataExpected, maxFECSourceSpan)
	}
	if sample.RepairBytes != 2*1200 {
		t.Fatalf("repair bytes = %d, want duplicate repair wire bytes counted", sample.RepairBytes)
	}
}

func TestLaneArrivalStatsKeepsOpenRepairGroupAsDataWaterline(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordRepair(transport.KindTCP, 0, maxFECSourceSpan, 1200, now.Add(3*time.Second))
	if _, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP); !ok {
		t.Fatal("missing first UDP sample")
	}

	sample, ok := findQoSSample(stats.qosSamples(now.Add(6*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing second UDP sample for still-open repair group")
	}
	if sample.DataExpected != maxFECSourceSpan || sample.DataArrived != 0 {
		t.Fatalf("second sample counts = got expected %d arrived %d, want open group expected=%d arrived=0",
			sample.DataExpected, sample.DataArrived, maxFECSourceSpan)
	}
}

func TestLaneArrivalStatsReportsRepairLossFromDataWaterline(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	for packetID := uint32(0); packetID < 100; packetID++ {
		stats.recordData(transport.KindTCP, packetID, 1000, now.Add(3*time.Second))
	}

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing UDP repair-leg sample")
	}
	if sample.DataExpected != 100 || sample.DataArrived != 0 {
		t.Fatalf("repair loss sample counts = got expected %d arrived %d, want expected=100 arrived=0", sample.DataExpected, sample.DataArrived)
	}
	if sample.DataBytes != 0 {
		t.Fatalf("repair loss sample bytes = %d, want 0", sample.DataBytes)
	}
}

func TestLaneArrivalStatsDoesNotTreatSessionWidePacketGapAsLaneLoss(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordData(transport.KindTCP, 10, 1000, now.Add(3*time.Second))
	stats.recordData(transport.KindTCP, 110, 1000, now.Add(3*time.Second))

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindTCP)
	if !ok {
		t.Fatal("missing TCP data sample")
	}
	if sample.DataExpected != 2 || sample.DataArrived != 2 {
		t.Fatalf("sample counts = got expected %d arrived %d, want only observed lane packets expected=2 arrived=2",
			sample.DataExpected, sample.DataArrived)
	}
}

func TestLaneArrivalStatsInfersRepairExpectationFromUnalignedDataGroup(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	for packetID := uint32(5); packetID < 9; packetID++ {
		stats.recordData(transport.KindTCP, packetID, 1000, now.Add(3*time.Second))
	}

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing UDP repair-leg sample")
	}
	if sample.DataExpected != 4 || sample.DataArrived != 0 {
		t.Fatalf("repair loss sample counts = got expected %d arrived %d, want expected=4 arrived=0", sample.DataExpected, sample.DataArrived)
	}
}

func TestLaneArrivalStatsDoesNotExpectRepairForPartialDataRun(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordData(transport.KindTCP, 20, 1000, now.Add(3*time.Second))
	stats.recordData(transport.KindTCP, 21, 1000, now.Add(3*time.Second))

	samples := stats.qosSamples(now.Add(3 * time.Second))
	if sample, ok := findQoSSample(samples, transport.KindUDP); ok && sample.DataExpected != 0 {
		t.Fatalf("repair sample = %+v, want no expected repair for partial DATA run without repair proof", sample)
	}
}

func TestLaneArrivalStatsAcceptsFlushSpanFromRepairHeader(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordData(transport.KindTCP, 30, 1000, now.Add(3*time.Second))
	stats.recordData(transport.KindTCP, 31, 1000, now.Add(3*time.Second))
	stats.recordRepair(transport.KindUDP, 30, 2, 1200, now.Add(3*time.Second))

	sample, ok := findQoSSample(stats.qosSamples(now.Add(3*time.Second)), transport.KindUDP)
	if !ok {
		t.Fatal("missing UDP repair sample")
	}
	if sample.DataExpected != 2 || sample.DataArrived != 2 {
		t.Fatalf("flush repair sample counts = got expected %d arrived %d, want expected=2 arrived=2",
			sample.DataExpected, sample.DataArrived)
	}
}

func TestLaneArrivalStatsComputesGroupLagMedian(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordRepair(transport.KindUDP, 0, maxFECSourceSpan, 1200, now.Add(3*time.Second))
	for packetID := uint32(0); packetID < maxFECSourceSpan; packetID++ {
		stats.recordData(transport.KindTCP, packetID, 1000, now.Add(3900*time.Millisecond))
	}

	sample, ok := findQoSSample(stats.qosSamples(now.Add(4*time.Second)), transport.KindTCP)
	if !ok {
		t.Fatal("missing TCP data-leg sample")
	}
	if sample.Lag != 900*time.Millisecond {
		t.Fatalf("lag = %s, want 900ms", sample.Lag)
	}
}

func TestLaneArrivalStatsComputesOpenGroupLagUpperBound(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.qosSamples(now)

	stats.recordRepair(transport.KindUDP, 0, maxFECSourceSpan, 1200, now.Add(time.Second))
	stats.recordData(transport.KindTCP, 0, 1000, now.Add(1200*time.Millisecond))

	sample, ok := findQoSSample(stats.qosSamples(now.Add(4*time.Second)), transport.KindTCP)
	if !ok {
		t.Fatal("missing TCP data-leg sample")
	}
	if sample.Lag != 3*time.Second {
		t.Fatalf("lag = %s, want 3s open-group upper bound", sample.Lag)
	}
}

func findQoSSample(samples []qosSample, kind transport.Kind) (qosSample, bool) {
	for _, sample := range samples {
		if sample.DataKind == kind {
			return sample, true
		}
	}
	return qosSample{}, false
}
