package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func observeRateTick(e *qosEstimator, at time.Time, dataKind, repairKind transport.Kind, dataBytes, repairBytes uint64) []qosStatus {
	var out []qosStatus
	out = append(out, e.ObserveRate(qosRateSample{
		At:          at,
		DataKind:    dataKind,
		RepairKind:  repairKind,
		DataBytes:   dataBytes,
		RepairBytes: repairBytes,
	})...)
	out = append(out, e.Tick(at.Add(time.Second))...)
	return out
}

func observeProfileRateTick(e *qosEstimator, at time.Time, dataKind, repairKind transport.Kind, dataBytes, repairBytes, profileDataBytes, profileRepairBytes uint64) []qosStatus {
	var out []qosStatus
	out = append(out, e.ObserveRate(qosRateSample{
		At:                 at,
		DataKind:           dataKind,
		RepairKind:         repairKind,
		DataBytes:          dataBytes,
		RepairBytes:        repairBytes,
		ProfileDataBytes:   profileDataBytes,
		ProfileRepairBytes: profileRepairBytes,
	})...)
	out = append(out, e.Tick(at.Add(time.Second))...)
	return out
}

func observeHealth(e *qosEstimator, at time.Time, dataKind, repairKind transport.Kind, arrived, expected uint64) {
	e.ObserveHealth(qosHealthSample{
		At:           at,
		DataKind:     dataKind,
		RepairKind:   repairKind,
		DataArrived:  arrived,
		DataExpected: expected,
	})
}

func TestQoSEstimatorStaysSilentBelowSampleFloor(t *testing.T) {
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 100, Sustain: time.Second, Tick: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	got = append(got, observeRateTick(e, time.Unix(0, 0), transport.KindUDP, transport.KindTCP, 4800, 1200)...)

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none below sample floor", got)
	}
}

func TestQoSEstimatorIgnoresSameKindRateSamples(t *testing.T) {
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 4; i++ {
		at := time.Unix(0, 0).Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindUDP, 4800, 1200)...)
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none from same-leg samples", got)
	}
}

func TestQoSEstimatorRateEventsUpdateStreams(t *testing.T) {
	now := time.Unix(0, 0)
	state := &qosDirectionState{dataKind: transport.KindUDP, repairKind: transport.KindTCP}

	state.observeRate(qosRateSample{
		At:          now,
		DataKind:    transport.KindUDP,
		RepairKind:  transport.KindTCP,
		DataBytes:   4800,
		RepairBytes: 1200,
	}, now)
	if state.flush(now, time.Second) {
		t.Fatal("first rate event should establish tick baseline only")
	}
	if !state.flush(now.Add(time.Second), time.Second) {
		t.Fatal("tick should initialize EMA streams from pending events")
	}

	if !state.actual.initialized || !state.repair.initialized {
		t.Fatal("actual and repair rate EMAs should be initialized")
	}
	if state.lastActual != 4800 || state.lastRepair != 1200 {
		t.Fatalf("last bytes = actual %d repair %d, want 4800/1200", state.lastActual, state.lastRepair)
	}
	if state.groupBytes.initialized || state.repairBytes.initialized {
		t.Fatal("flush should not train DATA/REPAIR byte profiles directly")
	}
	state.observeProfile()
	if !state.groupBytes.initialized || !state.repairBytes.initialized {
		t.Fatal("paired tick should be able to initialize DATA and REPAIR byte profiles")
	}
}

func TestQoSEstimatorRateEventsDecayUnobservedStreams(t *testing.T) {
	now := time.Unix(0, 0)
	state := &qosDirectionState{dataKind: transport.KindUDP, repairKind: transport.KindTCP}

	state.observeRate(qosRateSample{
		At:          now,
		DataKind:    transport.KindUDP,
		RepairKind:  transport.KindTCP,
		DataBytes:   4800,
		RepairBytes: 1200,
	}, now)
	state.flush(now, time.Second)
	state.observeRate(qosRateSample{
		At:          now.Add(time.Second),
		DataKind:    transport.KindUDP,
		RepairKind:  transport.KindTCP,
		DataBytes:   4800,
		RepairBytes: 1200,
	}, now.Add(time.Second))
	if !state.flush(now.Add(time.Second), time.Second) {
		t.Fatal("second rate event should initialize EMA streams")
	}
	repairBefore := state.repair.value

	state.observeRate(qosRateSample{
		At:         now.Add(2 * time.Second),
		DataKind:   transport.KindUDP,
		RepairKind: transport.KindTCP,
		DataBytes:  4800,
	}, now.Add(2*time.Second))
	if !state.flush(now.Add(2*time.Second), time.Second) {
		t.Fatal("DATA-only event should update rate streams")
	}

	if state.repair.value >= repairBefore {
		t.Fatalf("repair EMA = %.0f, want below %.0f after missing repair bytes", state.repair.value, repairBefore)
	}
}

func TestQoSEstimatorEmptyTicksDecayRateStreams(t *testing.T) {
	now := time.Unix(0, 0)
	state := &qosDirectionState{dataKind: transport.KindUDP, repairKind: transport.KindTCP}

	state.observeRate(qosRateSample{
		At:          now,
		DataKind:    transport.KindUDP,
		RepairKind:  transport.KindTCP,
		DataBytes:   4800,
		RepairBytes: 1200,
	}, now)
	state.flush(now, time.Second)
	if !state.flush(now.Add(time.Second), time.Second) {
		t.Fatal("initial tick should initialize EMA streams")
	}
	actualBefore := state.actual.value
	repairBefore := state.repair.value

	if !state.flush(now.Add(2*time.Second), time.Second) {
		t.Fatal("empty tick should still update initialized EMA streams")
	}
	if state.actual.value >= actualBefore {
		t.Fatalf("actual EMA = %.0f, want below %.0f after empty tick", state.actual.value, actualBefore)
	}
	if state.repair.value >= repairBefore {
		t.Fatalf("repair EMA = %.0f, want below %.0f after empty tick", state.repair.value, repairBefore)
	}
}

func TestQoSEstimatorEmptyTicksCanSustainExistingRateGap(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	state := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	state.groupBytes.observe(4800)
	state.repairBytes.observe(1200)

	got = append(got, observeRateTick(e, now, transport.KindUDP, transport.KindTCP, 1200, 1200)...)
	for i := 1; i < 5; i++ {
		got = append(got, e.Tick(now.Add(time.Duration(i+1)*time.Second))...)
	}

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one sustained limited status from empty ticks", got)
	}
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", got[0])
	}
}

func TestQoSEstimatorMarksDataLegLimitedFromRateGap(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	state := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	state.groupBytes.observe(4800)
	state.repairBytes.observe(1200)

	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 1200, 1200)...)
	}

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one limited snapshot", got)
	}
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", got[0])
	}
	if got[0].UDPDeliveredBps == 0 {
		t.Fatalf("status = %+v, want UDP delivered bps populated", got[0])
	}
}

func TestQoSEstimatorMarksDataLegLimitedWhenDataMissing(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 1200)...)
	}
	for i := 2; i < 8; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 0, 1200)...)
	}

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one limited snapshot", got)
	}
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", got[0])
	}
}

func TestQoSEstimatorUsesRepairProfileForDataLegLimit(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	// A sparse warmup group can legitimately have a 1:1 DATA/REPAIR profile.
	// Later loaded groups carry four DATA shards per REPAIR shard; that FEC
	// structure must replace the warmup profile instead of leaving expected DATA
	// bytes pinned to the REPAIR byte rate.
	got = append(got, observeProfileRateTick(e, now, transport.KindUDP, transport.KindTCP, 168, 168, 168, 168)...)
	for i := 1; i < 6; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeProfileRateTick(e, at, transport.KindUDP, transport.KindTCP, 1200, 1200, 4800, 1200)...)
	}

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one limited snapshot", got)
	}
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", got[0])
	}
}

func TestQoSEstimatorDataLimitedDoesNotGateReverseDirectionDataLimited(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	tcpData := e.direction(transport.KindTCP, transport.KindUDP, qosRoleData)

	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now); ok {
		got = append(got, status)
	}
	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now.Add(1100*time.Millisecond)); ok {
		got = append(got, status)
	}
	if len(got) != 1 || !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("initial statuses = %+v, want UDP limited only", got)
	}

	if status, ok := e.driveLimitState(tcpData, qosLimitStateDataRate, true, 100, now.Add(1200*time.Millisecond)); ok {
		got = append(got, status)
	}
	if status, ok := e.driveLimitState(tcpData, qosLimitStateDataRate, true, 100, now.Add(2300*time.Millisecond)); ok {
		got = append(got, status)
	}
	if len(got) != 2 {
		t.Fatalf("statuses = %+v, want UDP limited then TCP limited", got)
	}
	if !got[1].UDPLimited || !got[1].TCPLimited {
		t.Fatalf("status = %+v, want both UDP and TCP limited", got[1])
	}
}

func TestQoSEstimatorDoesNotEmitDeliveredBpsOnlyWhenOneLegLimited(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)

	status, ok := e.commitLimitState(udpData, qosLimitStateDataRate, true, 1000, now, now)
	if !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("initial status = %+v ok=%t, want UDP limited only", status, ok)
	}

	status, ok = e.commitLimitState(udpData, qosLimitStateDataRate, true, 2000, now.Add(time.Second), now.Add(time.Second))
	if ok {
		t.Fatalf("bps-only status = %+v, want no emitted snapshot while only UDP is limited", status)
	}
	if got := e.snapshot().UDPDeliveredBps; got != 2000 {
		t.Fatalf("local UDP delivered bps = %d, want 2000", got)
	}
}

func TestQoSEstimatorEmitsBothLimitedWhenDeliveredBpsFlipsPreferred(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	tcpData := e.direction(transport.KindTCP, transport.KindUDP, qosRoleData)

	if status, ok := e.commitLimitState(udpData, qosLimitStateDataRate, true, 1000, now, now); !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("UDP limited status = %+v ok=%t, want UDP limited only", status, ok)
	}
	status, ok := e.commitLimitState(tcpData, qosLimitStateDataRate, true, 2000, now.Add(time.Second), now.Add(time.Second))
	if !ok || !status.UDPLimited || !status.TCPLimited || e.currentPrimary != transport.KindTCP {
		t.Fatalf("both-limited status = %+v ok=%t primary=%v, want TCP preferred", status, ok, e.currentPrimary)
	}

	status, ok = e.commitLimitState(udpData, qosLimitStateDataRate, true, 3000, now.Add(2*time.Second), now.Add(2*time.Second))
	if !ok {
		t.Fatal("expected emitted snapshot when both-limited delivered bps flips preferred leg")
	}
	if !status.UDPLimited || !status.TCPLimited || status.UDPDeliveredBps != 3000 || status.TCPDeliveredBps != 2000 {
		t.Fatalf("preferred-flip status = %+v, want both limited with updated bps", status)
	}
	if e.currentPrimary != transport.KindUDP {
		t.Fatalf("current primary = %v, want UDP after updated bps becomes preferred", e.currentPrimary)
	}
}

func TestQoSEstimatorKindClearResetsRelatedDirectionPending(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	tcpData := e.direction(transport.KindTCP, transport.KindUDP, qosRoleData)
	tcpData.dataRatePending = qosPending{
		active:  true,
		limited: true,
		since:   now.Add(-10 * time.Second),
		bps:     100,
	}
	tcpData.shadowPending = qosPending{
		active:  true,
		limited: false,
		since:   now.Add(-10 * time.Second),
		bps:     100,
	}

	e.clearLimitStateForKind(transport.KindUDP)

	if tcpData.dataRatePending.active || tcpData.shadowPending.active {
		t.Fatalf("pending after UDP clear = data %+v shadow %+v, want both cleared", tcpData.dataRatePending, tcpData.shadowPending)
	}
}

func TestQoSEstimatorDataRoleDoesNotMarkShadowLegLimitedWhenRepairMissing(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 1200)...)
	}
	for i := 2; i < 5; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 0)...)
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none because DATA role does not judge shadow limited", got)
	}
}

func TestQoSEstimatorIgnoresNonCurrentPrimarySamplesUntilCommittedState(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: 3 * time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	tcpShadow := e.direction(transport.KindTCP, transport.KindUDP, qosRoleShadow)
	udpData.groupBytes.observe(4800)
	udpData.repairBytes.observe(1200)

	got = append(got, observeRateTick(e, now, transport.KindUDP, transport.KindTCP, 1200, 1200)...)
	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want pending only", got)
	}

	e.ObserveRate(qosRateSample{
		At:         now.Add(1500 * time.Millisecond),
		DataKind:   transport.KindTCP,
		RepairKind: transport.KindUDP,
		DataBytes:  4800,
	})
	if e.currentPrimary != transport.KindUDP {
		t.Fatalf("current primary = %v, want UDP while UDP limited pending is unresolved", e.currentPrimary)
	}
	if tcpShadow.sampleTotal != 0 {
		t.Fatalf("TCP shadow samples = %d, want flight sample ignored before committed switch", tcpShadow.sampleTotal)
	}

	for i := 2; i <= 4; i++ {
		got = append(got, e.Tick(now.Add(time.Duration(i)*time.Second))...)
	}
	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want UDP limited after pending sustain", got)
	}
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", got[0])
	}
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("current primary = %v, want TCP after UDP limited is committed", e.currentPrimary)
	}
}

func TestQoSEstimatorShadowRoleAvoidsReverseDataLimitedFalsePositive(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Hour, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	tcpData := e.direction(transport.KindTCP, transport.KindUDP, qosRoleData)
	tcpData.groupBytes.observe(4800)
	tcpData.repairBytes.observe(1200)

	e.currentPrimary = transport.KindUDP
	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now); ok {
		got = append(got, status)
	}
	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now.Add(time.Hour)); ok {
		got = append(got, status)
	}
	if len(got) != 1 || !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("initial statuses = %+v, want UDP limited only", got)
	}

	for i := 4; i < 8; i++ {
		at := now.Add(time.Hour + time.Duration(i)*time.Second)
		got = append(got, observeRateTick(e, at, transport.KindTCP, transport.KindUDP, 4200, 1200)...)
	}
	for _, status := range got {
		if status.TCPLimited {
			t.Fatalf("statuses = %+v, want no TCP limited while observing UDP shadow", got)
		}
	}
	if tcpData.sampleTotal != 0 {
		t.Fatalf("TCP DATA role samples = %d, want 0 while UDP shadow is current role", tcpData.sampleTotal)
	}
}

func TestQoSEstimatorPrimarySwitchPreservesDirectionProfile(t *testing.T) {
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	udpData.groupBytes.observe(4800)
	udpData.repairBytes.observe(1200)
	udpData.pidCorrection = 42
	udpData.pidIntegral = 7
	udpData.pidPrevError = 3

	e.currentPrimary = transport.KindTCP
	tcpData := e.direction(transport.KindTCP, transport.KindUDP, qosRoleData)
	if status, ok := e.driveLimitState(tcpData, qosLimitStateDataRate, true, 100, time.Unix(0, 0)); ok {
		t.Fatalf("first limited state = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(tcpData, qosLimitStateDataRate, true, 100, time.Unix(1, 0))
	if !ok || !status.TCPLimited || status.UDPLimited {
		t.Fatalf("limited status = %+v ok=%t, want TCP limited only", status, ok)
	}
	if e.currentPrimary != transport.KindUDP {
		t.Fatalf("current primary = %v, want UDP after TCP limited is committed", e.currentPrimary)
	}

	if !udpData.groupBytes.initialized || !udpData.repairBytes.initialized {
		t.Fatal("direction profile was cleared on primary switch")
	}
	if udpData.groupBytes.value != 4800 || udpData.repairBytes.value != 1200 {
		t.Fatalf("direction profile = %.0f/%.0f, want 4800/1200", udpData.groupBytes.value, udpData.repairBytes.value)
	}
	if udpData.pidCorrection != 42 || udpData.pidIntegral != 7 || udpData.pidPrevError != 3 {
		t.Fatalf("pid state = %.0f/%.0f/%.0f, want 42/7/3", udpData.pidCorrection, udpData.pidIntegral, udpData.pidPrevError)
	}
}

func TestQoSEstimatorLateDataAfterLimitedDoesNotEraseCommittedState(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	e.currentPrimary = transport.KindUDP
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)

	if status, ok := e.driveLimitState(udpData, qosLimitStateHealth, true, 100, now); ok {
		t.Fatalf("first limited state = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(udpData, qosLimitStateHealth, true, 100, now.Add(1100*time.Millisecond))
	if !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("limited status = %+v ok=%t, want UDP limited", status, ok)
	}

	if e.currentPrimary != transport.KindTCP || e.currentRoleLocked() != qosRoleShadow {
		t.Fatalf("after UDP limited primary=%v role=%v, want TCP shadow role", e.currentPrimary, e.currentRoleLocked())
	}

	e.ObserveRate(qosRateSample{
		At:         now.Add(1300 * time.Millisecond),
		DataKind:   transport.KindUDP,
		RepairKind: transport.KindTCP,
		DataBytes:  1200,
	})
	if !e.kindLimitedLocked(transport.KindUDP) {
		t.Fatal("late UDP DATA erased committed UDP limited state")
	}
	if e.currentPrimary != transport.KindTCP || e.currentRoleLocked() != qosRoleShadow {
		t.Fatalf("after late DATA primary=%v role=%v, want TCP shadow role", e.currentPrimary, e.currentRoleLocked())
	}

	e.ObserveRate(qosRateSample{
		At:         now.Add(1400 * time.Millisecond),
		DataKind:   transport.KindTCP,
		RepairKind: transport.KindUDP,
		DataBytes:  4800,
	})
	if e.currentPrimary != transport.KindTCP || e.currentRoleLocked() != qosRoleShadow {
		t.Fatalf("after late DATA primary=%v role=%v, want TCP shadow role", e.currentPrimary, e.currentRoleLocked())
	}
}

func TestQoSEstimatorDoesNotMarkHealthyRateEvents(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	for i := 0; i < 8; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 1200)...)
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none for healthy DATA/REPAIR events", got)
	}
}

func TestQoSEstimatorDoesNotLearnProfileFromRateLimitedGap(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	state := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	state.groupBytes.observe(4800)
	state.repairBytes.observe(1200)

	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 1200, 1200)
	}

	if state.groupBytes.value != 4800 || state.repairBytes.value != 1200 {
		t.Fatalf("profile = %.0f/%.0f, want unchanged 4800/1200", state.groupBytes.value, state.repairBytes.value)
	}
}

func TestQoSEstimatorShadowCleanClearsHealthLimitedLeg(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	observeHealth(e, now, transport.KindUDP, transport.KindTCP, 0, 4)
	got = append(got, e.Tick(now.Add(time.Second))...)
	observeHealth(e, now.Add(2*time.Second), transport.KindUDP, transport.KindTCP, 0, 4)
	got = append(got, e.Tick(now.Add(2*time.Second))...)
	observeHealth(e, now.Add(3*time.Second), transport.KindUDP, transport.KindTCP, 0, 4)
	got = append(got, e.Tick(now.Add(3*time.Second))...)
	if len(got) != 1 || !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("initial statuses = %+v, want UDP health-limited", got)
	}

	for i := 0; i < 16; i++ {
		at := now.Add(time.Duration(4+i) * time.Second)
		got = append(got, observeRateTick(e, at, transport.KindTCP, transport.KindUDP, 4800, 1200)...)
	}

	if len(got) != 2 {
		t.Fatalf("statuses = %+v, want limited then clear", got)
	}
	if got[1].UDPLimited || got[1].TCPLimited {
		t.Fatalf("clear status = %+v, want both legs clear", got[1])
	}
}

func TestQoSEstimatorShadowCleanResetsStalePendingState(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	dataDir := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	shadowDir := e.direction(transport.KindTCP, transport.KindUDP, qosRoleShadow)

	if status, ok := e.driveLimitState(dataDir, qosLimitStateDataRate, true, 100, now); ok {
		t.Fatalf("first limited state = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(dataDir, qosLimitStateDataRate, true, 100, now.Add(1100*time.Millisecond))
	if !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("limited status = %+v ok=%t, want UDP limited", status, ok)
	}

	if status, ok = e.driveLimitState(dataDir, qosLimitStateHealth, true, 100, now.Add(1200*time.Millisecond)); ok {
		t.Fatalf("health state = %+v, want stale pending only", status)
	}
	if status, ok = e.driveLimitState(shadowDir, qosLimitStateShadow, false, 100, now.Add(1300*time.Millisecond)); ok {
		t.Fatalf("first shadow clean state = %+v, want pending only", status)
	}
	status, ok = e.driveLimitState(shadowDir, qosLimitStateShadow, false, 100, now.Add(2400*time.Millisecond))
	if !ok || status.UDPLimited || status.TCPLimited {
		t.Fatalf("clear status = %+v ok=%t, want both legs clear", status, ok)
	}

	if status, ok = e.driveLimitState(dataDir, qosLimitStateHealth, true, 100, now.Add(2500*time.Millisecond)); ok {
		t.Fatalf("stale health state after shadow clean = %+v, want pending reset", status)
	}
}

func TestQoSEstimatorHealthCanMarkDespiteHealthyRate(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	var got []qosStatus
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * 2 * time.Second)
		observeHealth(e, at, transport.KindUDP, transport.KindTCP, 0, 4)
		got = append(got, observeRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 1200)...)
	}

	if len(got) != 1 || !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("statuses = %+v, want UDP health-limited despite healthy rate estimate", got)
	}
}

func TestQoSEstimatorHealthMarksDataLegAndShadowClearRestoresIt(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second}, nil)

	observeHealth(e, now, transport.KindUDP, transport.KindTCP, 0, 4)
	if statuses := e.Tick(now.Add(time.Second)); len(statuses) != 0 {
		t.Fatalf("first health tick statuses = %+v, want pending only", statuses)
	}
	observeHealth(e, now.Add(2*time.Second), transport.KindUDP, transport.KindTCP, 0, 4)
	statuses := e.Tick(now.Add(2 * time.Second))
	if len(statuses) != 0 {
		t.Fatalf("second health tick statuses = %+v, want pending only", statuses)
	}
	observeHealth(e, now.Add(3*time.Second), transport.KindUDP, transport.KindTCP, 0, 4)
	statuses = e.Tick(now.Add(3 * time.Second))
	if len(statuses) != 1 || !statuses[0].UDPLimited {
		t.Fatalf("limited statuses = %+v, want UDP limited", statuses)
	}
	if e.currentPrimary != transport.KindTCP || e.currentRoleLocked() != qosRoleShadow {
		t.Fatalf("after UDP limited primary=%v role=%v, want TCP shadow role", e.currentPrimary, e.currentRoleLocked())
	}

	shadow := e.direction(transport.KindTCP, transport.KindUDP, qosRoleShadow)
	if status, ok := e.driveLimitState(shadow, qosLimitStateShadow, false, 100, now.Add(4*time.Second)); ok {
		t.Fatalf("first shadow clear status = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(shadow, qosLimitStateShadow, false, 100, now.Add(5*time.Second))
	if !ok || status.UDPLimited || status.TCPLimited {
		t.Fatalf("shadow clear status = %+v ok=%t, want both legs clear", status, ok)
	}
	if e.currentPrimary != transport.KindUDP || e.currentRoleLocked() != qosRoleData {
		t.Fatalf("after UDP clear primary=%v role=%v, want UDP data role", e.currentPrimary, e.currentRoleLocked())
	}
}

func TestQoSEstimatorOwnsMatureTimer(t *testing.T) {
	group := rxGroupKey{basePacketID: 10, sourceSpan: 4}
	matured := make(chan rxGroupKey, 1)
	e := newQoSEstimator(qosConfig{
		MatureAfter: 10 * time.Millisecond,
		Mature: func(group rxGroupKey, at time.Time) rxWindowResult {
			matured <- group
			return rxWindowResult{}
		},
	}, nil)
	defer e.Close()

	e.ObserveResult(rxWindowResult{matureGroup: group, hasMatureGroup: true})

	select {
	case got := <-matured:
		if got != group {
			t.Fatalf("mature group = %+v, want %+v", got, group)
		}
	case <-time.After(time.Second):
		t.Fatal("mature timer did not fire")
	}
}

func TestQoSEstimatorCancelsMatureTimerOnCompleteGroup(t *testing.T) {
	group := rxGroupKey{basePacketID: 10, sourceSpan: 4}
	matured := make(chan rxGroupKey, 1)
	e := newQoSEstimator(qosConfig{
		Sustain:     time.Second,
		MatureAfter: 20 * time.Millisecond,
		Mature: func(group rxGroupKey, at time.Time) rxWindowResult {
			matured <- group
			return rxWindowResult{}
		},
	}, nil)
	defer e.Close()

	e.ObserveResult(rxWindowResult{matureGroup: group, hasMatureGroup: true})
	e.ObserveResult(rxWindowResult{completeGroup: group, hasComplete: true})

	select {
	case got := <-matured:
		t.Fatalf("mature timer fired for completed group %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestQoSEstimatorCancelsMatureTimersOnCommittedPrimarySwitch(t *testing.T) {
	group := rxGroupKey{basePacketID: 10, sourceSpan: 4}
	matured := make(chan rxGroupKey, 1)
	e := newQoSEstimator(qosConfig{
		Sustain:     time.Second,
		MatureAfter: 20 * time.Millisecond,
		Mature: func(group rxGroupKey, at time.Time) rxWindowResult {
			matured <- group
			return rxWindowResult{}
		},
	}, nil)
	defer e.Close()

	e.ObserveResult(rxWindowResult{matureGroup: group, hasMatureGroup: true})
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, time.Unix(0, 0)); ok {
		t.Fatalf("first limited status = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, time.Unix(1, 0))
	if !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("limited status = %+v ok=%t, want UDP limited", status, ok)
	}

	select {
	case got := <-matured:
		t.Fatalf("mature timer fired after primary switch for group %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}
