package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestRxWindowProducesArrivalRateSamples(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	data := w.addData(transport.KindUDP, 0, make([]byte, 46), now)
	if len(data.rates) != 1 {
		t.Fatalf("DATA rates = %+v, want one", data.rates)
	}
	if data.rates[0].DataKind != transport.KindUDP || data.rates[0].RepairKind != transport.KindTCP {
		t.Fatalf("DATA rate kinds = data %d repair %d, want UDP/TCP", data.rates[0].DataKind, data.rates[0].RepairKind)
	}
	if data.rates[0].DataBytes != 46 || data.rates[0].RepairBytes != 0 {
		t.Fatalf("DATA rate = %+v, want DATA bytes only", data.rates[0])
	}

	repair := w.addRepair(transport.KindTCP, 0, 7, 1, make([]byte, 64), now.Add(time.Millisecond))
	if len(repair.rates) != 1 {
		t.Fatalf("REPAIR rates = %+v, want one", repair.rates)
	}
	if repair.rates[0].DataKind != transport.KindUDP || repair.rates[0].RepairKind != transport.KindTCP {
		t.Fatalf("REPAIR rate kinds = data %d repair %d, want UDP/TCP", repair.rates[0].DataKind, repair.rates[0].RepairKind)
	}
	if repair.rates[0].DataBytes != 0 || repair.rates[0].RepairBytes != 64 {
		t.Fatalf("REPAIR rate = %+v, want REPAIR bytes only", repair.rates[0])
	}
	if repair.rates[0].ProfileDataBytes != 0 || repair.rates[0].ProfileRepairBytes != 0 {
		t.Fatalf("REPAIR profile = %+v, want no profile bytes from repair arrival", repair.rates[0])
	}
}

func TestRxWindowCompleteGroupDoesNotProduceQoSSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	for i := uint32(0); i < 4; i++ {
		result := w.addData(transport.KindUDP, i, make([]byte, 10+i), now.Add(time.Duration(i)*time.Millisecond))
		if result.hasRecoverable {
			t.Fatalf("DATA %d result = %+v, want no recovery before REPAIR", i, result)
		}
		if len(result.rates) != 1 {
			t.Fatalf("DATA %d rates = %+v, want one arrival rate", i, result.rates)
		}
	}

	result := w.addRepair(transport.KindTCP, 0, 7, 4, make([]byte, 64), now.Add(10*time.Millisecond))
	if result.hasRecoverable {
		t.Fatal("complete group should not request recovery")
	}
	if len(result.rates) != 1 || result.rates[0].RepairBytes != 64 {
		t.Fatalf("REPAIR result rates = %+v, want one REPAIR arrival rate", result.rates)
	}
	if result.rates[0].ProfileDataBytes != 0 || result.rates[0].ProfileRepairBytes != 0 {
		t.Fatalf("REPAIR profile = %+v, want no profile bytes from repair arrival", result.rates[0])
	}
	if !result.hasComplete {
		t.Fatal("complete group should cancel mature tracking")
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after complete group = %d, want 0", len(w.repairs))
	}

	again := w.addRepair(transport.KindTCP, 0, 8, 4, make([]byte, 64), now.Add(11*time.Millisecond))
	if len(again.rates) != 0 || again.hasRecoverable {
		t.Fatalf("duplicate closed-group repair result = %+v, want no rate/recovery", again)
	}
}

func TestRxWindowPostRepairDATACompletesGroupWithoutQoSSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	repair := w.addRepair(transport.KindTCP, 10, 9, 4, make([]byte, 64), now)
	if repair.hasRecoverable {
		t.Fatalf("empty repair result = %+v, want no recovery", repair)
	}
	if len(repair.rates) != 1 || repair.rates[0].RepairBytes != 64 {
		t.Fatalf("repair result rates = %+v, want one REPAIR arrival rate", repair.rates)
	}
	for i := uint32(0); i < 3; i++ {
		result := w.addData(transport.KindUDP, 10+i, make([]byte, 20), now.Add(time.Duration(i+1)*time.Millisecond))
		if len(result.rates) != 1 || result.rates[0].DataBytes != 20 {
			t.Fatalf("DATA %d rates = %+v, want one DATA arrival rate", i, result.rates)
		}
	}

	result := w.addData(transport.KindUDP, 13, make([]byte, 20), now.Add(4*time.Millisecond))
	if result.hasRecoverable {
		t.Fatal("complete group should not be recoverable")
	}
	if len(result.rates) != 1 || result.rates[0].DataBytes != 20 {
		t.Fatalf("final DATA rates = %+v, want one DATA arrival rate", result.rates)
	}
	if !result.hasComplete {
		t.Fatal("post-repair DATA should complete the repair-proven group")
	}
}

func TestRxWindowMatureIncompleteGroupProducesHealthOnly(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	w.addData(transport.KindUDP, 20, make([]byte, 40), now)
	w.addData(transport.KindUDP, 22, make([]byte, 60), now.Add(time.Millisecond))
	repair := w.addRepair(transport.KindTCP, 20, 9, 4, make([]byte, 100), now.Add(2*time.Millisecond))
	if len(repair.rates) != 1 {
		t.Fatalf("repair rate samples = %+v, want one", repair.rates)
	}
	rate := repair.rates[0]
	if rate.DataKind != transport.KindUDP || rate.RepairKind != transport.KindTCP {
		t.Fatalf("rate kinds = data %d repair %d, want UDP/TCP", rate.DataKind, rate.RepairKind)
	}
	if rate.RepairBytes != 100 {
		t.Fatalf("rate RepairBytes = %d, want repair arrival bytes 100", rate.RepairBytes)
	}
	if rate.ProfileDataBytes != 0 || rate.ProfileRepairBytes != 0 {
		t.Fatalf("rate profile = %+v, want no profile bytes from repair arrival", rate)
	}
	if rate.DataBytes != 0 {
		t.Fatalf("repair arrival rate = %+v, want REPAIR-only rate event", rate)
	}

	mature := w.matureQoSGroup(repair.matureGroup, now.Add(1500*time.Millisecond))
	if len(mature.rates) != 0 {
		t.Fatalf("mature rates = %+v, want none", mature.rates)
	}
	if len(mature.health) != 1 {
		t.Fatalf("mature health = %+v, want one FEC health sample", mature.health)
	}
	if mature.health[0].DataExpected != 4 || mature.health[0].DataArrived != 2 {
		t.Fatalf("mature health counts = expected %d arrived %d, want 4/2", mature.health[0].DataExpected, mature.health[0].DataArrived)
	}

	data := w.addData(transport.KindUDP, 21, make([]byte, 50), now.Add(3*time.Millisecond))
	if len(data.rates) != 1 {
		t.Fatalf("post-repair DATA rate samples = %+v, want one", data.rates)
	}
	if data.rates[0].DataBytes != 50 || data.rates[0].RepairBytes != 0 {
		t.Fatalf("post-repair DATA rate = %+v, want DATA-only rate event", data.rates[0])
	}
}

func TestRxWindowRecoveredGroupDoesNotProduceQoSSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	w.addData(transport.KindUDP, 20, make([]byte, 40), now)
	w.addData(transport.KindUDP, 21, make([]byte, 41), now.Add(time.Millisecond))
	w.addData(transport.KindUDP, 23, make([]byte, 43), now.Add(2*time.Millisecond))

	result := w.addRepair(transport.KindTCP, 20, 5, 4, make([]byte, 64), now.Add(3*time.Millisecond))
	if !result.hasRecoverable {
		t.Fatal("group with one missing DATA should be recoverable")
	}
	if result.recoverable.missingIndex != 2 {
		t.Fatalf("missing index = %d, want 2", result.recoverable.missingIndex)
	}

	health := w.finishRecovery(result.recoverable, make([]byte, 37), now.Add(4*time.Millisecond))
	if len(health) != 0 {
		t.Fatalf("health samples = %+v, want none", health)
	}

	mature := w.matureQoSGroup(rxGroupKey{basePacketID: 20, sourceSpan: 4}, now.Add(1500*time.Millisecond))
	if len(mature.rates) != 0 || len(mature.health) != 0 || mature.hasRecoverable {
		t.Fatalf("mature recovered-group result = %+v, want no QoS sample/rate/health", mature)
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after recovery = %d, want 0", len(w.repairs))
	}
}

func TestRxWindowTCPRecoveredGroupDoesNotProduceQoSSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(1)

	result := w.addRepair(transport.KindUDP, 300, 9, 1, make([]byte, 64), now)
	if !result.hasRecoverable {
		t.Fatal("UDP repair with one missing TCP DATA should be recoverable")
	}

	health := w.finishRecovery(result.recoverable, make([]byte, 80), now.Add(10*time.Millisecond))
	if len(health) != 0 {
		t.Fatalf("health samples = %+v, want none", health)
	}
	if len(w.repairs) != 0 {
		t.Fatalf("repairs retained after recovery = %d, want 0", len(w.repairs))
	}

	mature := w.matureQoSGroup(result.matureGroup, now.Add(1500*time.Millisecond))
	if len(mature.rates) != 0 || len(mature.health) != 0 || mature.hasRecoverable {
		t.Fatalf("mature recovered-group result = %+v, want no QoS sample/rate/health", mature)
	}
}

func TestRxWindowDATAWithoutRepairOnlyProducesArrivalRate(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)

	for i := uint32(0); i < 4; i++ {
		result := w.addData(transport.KindUDP, 100+i, make([]byte, 20), now.Add(time.Duration(i)*time.Millisecond))
		if result.hasRecoverable || len(result.health) != 0 {
			t.Fatalf("DATA without REPAIR result = %+v, want no recovery/health", result)
		}
		if len(result.rates) != 1 || result.rates[0].DataBytes != 20 {
			t.Fatalf("DATA without REPAIR rates = %+v, want one DATA arrival rate", result.rates)
		}
	}

	w = newRxSLCWindow(4)
	w.addData(transport.KindUDP, 200, make([]byte, 20), now)
	w.addData(transport.KindUDP, 203, make([]byte, 20), now.Add(time.Millisecond))
	result := w.addRepair(transport.KindTCP, 200, 11, 4, make([]byte, 64), now.Add(2*time.Millisecond))
	if result.hasRecoverable {
		t.Fatalf("unrecoverable group result = %+v, want no recovery", result)
	}
	if len(result.rates) != 1 || result.rates[0].RepairBytes != 64 {
		t.Fatalf("unrecoverable group rates = %+v, want one REPAIR arrival rate", result.rates)
	}
}

func TestRxWindowPrunedUnrecoverableGroupProducesHealthSample(t *testing.T) {
	now := time.Unix(0, 0)
	w := newRxSLCWindow(4)
	w.maxRepairs = 1

	w.addData(transport.KindUDP, 100, make([]byte, 100), now)
	w.addData(transport.KindUDP, 103, make([]byte, 120), now.Add(time.Millisecond))
	first := w.addRepair(transport.KindTCP, 100, 11, 4, make([]byte, 128), now.Add(2*time.Millisecond))
	if first.hasRecoverable || len(first.health) != 0 {
		t.Fatalf("first unrecoverable result = %+v, want no immediate recovery/health", first)
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
	if first.hasRecoverable || len(first.health) != 0 {
		t.Fatalf("first repair result = %+v, want no immediate recovery/health", first)
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
