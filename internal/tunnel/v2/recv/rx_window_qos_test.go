package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestRxWindowCompleteGroupSampleFromRepair(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	for i := uint32(0); i < 4; i++ {
		result := w.addData(transport.KindUDP, i, make([]byte, 10+i), now.Add(time.Duration(i)*time.Millisecond))
		if result.hasSample || result.hasRecoverable {
			t.Fatalf("DATA %d result = %+v, want no sample/recovery before REPAIR", i, result)
		}
	}

	result := w.addRepair(transport.KindTCP, 0, 7, 4, make([]byte, 64), now.Add(10*time.Millisecond))
	sample, ok := result.sample, result.hasSample
	if !ok {
		t.Fatal("missing complete-group QoS sample")
	}
	if result.hasRecoverable {
		t.Fatal("complete group should not request recovery")
	}
	if sample.DataKind != transport.KindUDP || sample.RepairKind != transport.KindTCP {
		t.Fatalf("sample kinds = data %d repair %d, want UDP/TCP", sample.DataKind, sample.RepairKind)
	}
	if sample.DataExpected != 4 || sample.DataArrived != 4 {
		t.Fatalf("sample counts = expected %d arrived %d, want 4/4", sample.DataExpected, sample.DataArrived)
	}
	if sample.DataBytes != 46 {
		t.Fatalf("DataBytes = %d, want 46", sample.DataBytes)
	}
	if sample.RepairBytes != 64 {
		t.Fatalf("RepairBytes = %d, want 64", sample.RepairBytes)
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after complete sample = %d, want 0", len(w.repairs))
	}

	again := w.addRepair(transport.KindTCP, 0, 8, 4, make([]byte, 64), now.Add(11*time.Millisecond))
	if again.hasSample || again.hasRecoverable {
		t.Fatalf("duplicate closed-group repair result = %+v, want no sample/recovery", again)
	}
}

func TestRxWindowCompleteGroupSampleFromLateDATA(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	repair := w.addRepair(transport.KindTCP, 10, 9, 4, make([]byte, 64), now)
	if repair.hasSample || repair.hasRecoverable {
		t.Fatalf("empty repair result = %+v, want no sample/recovery", repair)
	}
	for i := uint32(0); i < 3; i++ {
		result := w.addData(transport.KindUDP, 10+i, make([]byte, 20), now.Add(time.Duration(i+1)*time.Millisecond))
		if result.hasSample {
			t.Fatalf("DATA %d produced sample before group completion", i)
		}
	}

	result := w.addData(transport.KindUDP, 13, make([]byte, 20), now.Add(4*time.Millisecond))
	sample, ok := result.sample, result.hasSample
	if !ok {
		t.Fatal("late DATA should complete the repair-proven group")
	}
	if result.hasRecoverable {
		t.Fatal("complete group should not be recoverable")
	}
	if sample.DataExpected != 4 || sample.DataArrived != 4 || sample.DataBytes != 80 {
		t.Fatalf("sample = %+v, want expected=4 arrived=4 bytes=80", sample)
	}
}

func TestRxWindowRecoveredGroupSampleUsesRecoveredPayloadLength(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	w.addData(transport.KindUDP, 20, make([]byte, 40), now)
	w.addData(transport.KindUDP, 21, make([]byte, 41), now.Add(time.Millisecond))
	w.addData(transport.KindUDP, 23, make([]byte, 43), now.Add(2*time.Millisecond))

	result := w.addRepair(transport.KindTCP, 20, 5, 4, make([]byte, 64), now.Add(3*time.Millisecond))
	if !result.hasRecoverable {
		t.Fatal("group with one missing DATA should be recoverable")
	}
	if result.hasSample {
		t.Fatal("recoverable group should wait for successful reconstruction before sampling")
	}
	if result.recoverable.missingIndex != 2 {
		t.Fatalf("missing index = %d, want 2", result.recoverable.missingIndex)
	}

	sample, health, ok := w.finishRecovery(result.recoverable, make([]byte, 37), now.Add(4*time.Millisecond))
	if !ok {
		t.Fatal("missing recovered-group QoS sample")
	}
	if len(health) != 0 {
		t.Fatalf("health samples = %+v, want none", health)
	}
	if sample.DataExpected != 4 || sample.DataArrived != 3 {
		t.Fatalf("sample counts = expected %d arrived %d, want 4/3", sample.DataExpected, sample.DataArrived)
	}
	if sample.DataBytes != 124 {
		t.Fatalf("DataBytes = %d, want direct DATA bytes 124", sample.DataBytes)
	}
	if sample.RecoveredBytes != 37 {
		t.Fatalf("RecoveredBytes = %d, want exact recovered payload length 37", sample.RecoveredBytes)
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after recovery sample = %d, want 0", len(w.repairs))
	}
}

func TestRxWindowDefersTCPRecoverySampleUntilLateData(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(1)

	result := w.addRepair(transport.KindUDP, 300, 9, 1, make([]byte, 64), now)
	if !result.hasRecoverable {
		t.Fatal("UDP repair with one missing TCP DATA should be recoverable")
	}

	sample, health, ok := w.finishRecovery(result.recoverable, make([]byte, 80), now.Add(10*time.Millisecond))
	if ok {
		t.Fatalf("recovery sample = %+v, want no TCP limited evidence before late DATA", sample)
	}
	if len(health) != 0 {
		t.Fatalf("health samples = %+v, want none", health)
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after recovery = %d, want 0", len(w.repairs))
	}

	late := w.observeLateData(transport.KindTCP, 300, make([]byte, 100), now.Add(900*time.Millisecond))
	sample, ok = late.sample, late.hasSample
	if !ok {
		t.Fatal("late TCP DATA should produce a QoS sample")
	}
	if sample.DataKind != transport.KindTCP || sample.RepairKind != transport.KindUDP {
		t.Fatalf("sample kinds = data %d repair %d, want TCP/UDP", sample.DataKind, sample.RepairKind)
	}
	if sample.DataExpected != 1 || sample.DataArrived != 1 {
		t.Fatalf("sample counts = expected %d arrived %d, want 1/1", sample.DataExpected, sample.DataArrived)
	}
	if sample.DataBytes != 100 {
		t.Fatalf("DataBytes = %d, want late DATA bytes 100", sample.DataBytes)
	}
	again := w.observeLateData(transport.KindTCP, 300, make([]byte, 100), now.Add(time.Second))
	if again.hasSample || again.hasRecoverable {
		t.Fatalf("second duplicate result = %+v, want no repeated sample", again)
	}
}

func TestRxWindowDoesNotEstimateWithoutFECProof(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	for i := uint32(0); i < 4; i++ {
		result := w.addData(transport.KindUDP, 100+i, make([]byte, 20), now.Add(time.Duration(i)*time.Millisecond))
		if result.hasSample || result.hasRecoverable {
			t.Fatalf("DATA without REPAIR result = %+v, want no QoS evidence", result)
		}
	}

	w = newRxSLCWindow(4)
	w.addData(transport.KindUDP, 200, make([]byte, 20), now)
	w.addData(transport.KindUDP, 203, make([]byte, 20), now.Add(time.Millisecond))
	result := w.addRepair(transport.KindTCP, 200, 11, 4, make([]byte, 64), now.Add(2*time.Millisecond))
	if result.hasSample || result.hasRecoverable {
		t.Fatalf("unrecoverable group result = %+v, want no sample/recovery", result)
	}
}

func TestRxWindowPrunedUnrecoverableGroupProducesHealthSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)
	w.maxRepairs = 1

	w.addData(transport.KindUDP, 100, make([]byte, 100), now)
	w.addData(transport.KindUDP, 103, make([]byte, 120), now.Add(time.Millisecond))
	first := w.addRepair(transport.KindTCP, 100, 11, 4, make([]byte, 128), now.Add(2*time.Millisecond))
	if first.hasSample || first.hasRecoverable || len(first.health) != 0 {
		t.Fatalf("first unrecoverable result = %+v, want no immediate sample/recovery/health", first)
	}

	second := w.addRepair(transport.KindTCP, 200, 12, 4, make([]byte, 128), now.Add(5*time.Second))
	if len(second.health) != 1 {
		t.Fatalf("health samples = %+v, want one pruned unrecoverable sample", second.health)
	}
	health := second.health[0]
	if health.DataKind != transport.KindUDP || health.RepairKind != transport.KindTCP {
		t.Fatalf("health kinds = data %d repair %d, want UDP/TCP", health.DataKind, health.RepairKind)
	}
	if health.DataExpected != 4 || health.DataArrived != 2 {
		t.Fatalf("health counts = expected %d arrived %d, want 4/2", health.DataExpected, health.DataArrived)
	}
	if health.DeliveredBps != 0 {
		t.Fatalf("DeliveredBps = %d, want 0 because unrecoverable group rate is not trustworthy", health.DeliveredBps)
	}
}

func TestRxWindowRepairProgressProducesUnrecoverableHealthSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	first := w.addRepair(transport.KindTCP, 100, 11, 4, make([]byte, 128), now)
	if first.hasSample || first.hasRecoverable || len(first.health) != 0 {
		t.Fatalf("first repair result = %+v, want no immediate sample/recovery/health", first)
	}

	second := w.addRepair(transport.KindTCP, 200, 12, 4, make([]byte, 128), now.Add(time.Second))
	if len(second.health) != 1 {
		t.Fatalf("health samples = %+v, want one stale unrecoverable sample", second.health)
	}
	health := second.health[0]
	if health.DataKind != transport.KindUDP || health.RepairKind != transport.KindTCP {
		t.Fatalf("health kinds = data %d repair %d, want UDP/TCP", health.DataKind, health.RepairKind)
	}
	if health.DataExpected != 4 || health.DataArrived != 0 {
		t.Fatalf("health counts = expected %d arrived %d, want 4/0", health.DataExpected, health.DataArrived)
	}
	if health.DeliveredBps != 0 {
		t.Fatalf("DeliveredBps = %d, want 0 when no DATA arrived", health.DeliveredBps)
	}
}

func TestRxWindowRepairProgressKeepsGroupsWithinGrace(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	w.addRepair(transport.KindTCP, 100, 11, 4, make([]byte, 128), now)
	withinGraceBase := uint32(100 + 4 + defaultRxSLCWindowRepairGrace - 1)
	result := w.addRepair(transport.KindTCP, withinGraceBase, 12, 4, make([]byte, 128), now.Add(time.Second))
	if len(result.health) != 0 {
		t.Fatalf("health samples = %+v, want none within stale grace", result.health)
	}
	if _, ok := w.repairs[100]; !ok {
		t.Fatal("old repair was pruned within stale grace")
	}
}

func TestRxWindowUDPRepairProgressDoesNotMarkTCPUnrecoverable(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	w.addRepair(transport.KindUDP, 100, 11, 4, make([]byte, 128), now)
	result := w.addRepair(transport.KindUDP, 200, 12, 4, make([]byte, 128), now.Add(time.Second))
	if len(result.health) != 0 {
		t.Fatalf("health samples = %+v, want none for UDP repair proving TCP DATA", result.health)
	}
	if _, ok := w.repairs[100]; !ok {
		t.Fatal("UDP repair was pruned before delayed TCP DATA could arrive")
	}
}
