package recv

import (
	"reflect"
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

func TestQoSEstimatorDirectionStateHasNoRateEMA(t *testing.T) {
	stateType := reflect.TypeOf(qosDirectionState{})
	for _, name := range []string{"actual", "repair"} {
		if _, ok := stateType.FieldByName(name); ok {
			t.Fatalf("qosDirectionState still has %s EMA state", name)
		}
	}
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
		t.Fatal("tick should publish raw rate state from pending events")
	}
	if state.lastActual != 4800 || state.lastRepair != 1200 {
		t.Fatalf("last bytes = actual %d repair %d, want 4800/1200", state.lastActual, state.lastRepair)
	}
	if state.lastActualBps != 38400 || state.lastRepairBps != 9600 {
		t.Fatalf("last bps = actual %d repair %d, want 38400/9600", state.lastActualBps, state.lastRepairBps)
	}
	if state.lastProfileData != 0 || state.lastProfileRepair != 0 {
		t.Fatalf("last profile bytes = data %d repair %d, want none without group profile", state.lastProfileData, state.lastProfileRepair)
	}
}

func TestQoSEstimatorRateEventsUseCurrentTickOnly(t *testing.T) {
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
		t.Fatal("second rate event should publish raw rate state")
	}

	state.observeRate(qosRateSample{
		At:         now.Add(2 * time.Second),
		DataKind:   transport.KindUDP,
		RepairKind: transport.KindTCP,
		DataBytes:  4800,
	}, now.Add(2*time.Second))
	if !state.flush(now.Add(2*time.Second), time.Second) {
		t.Fatal("DATA-only event should update rate streams")
	}

	if state.lastActual != 4800 || state.lastRepair != 0 {
		t.Fatalf("last bytes = actual %d repair %d, want 4800/0", state.lastActual, state.lastRepair)
	}
	if state.lastActualBps != 38400 || state.lastRepairBps != 0 {
		t.Fatalf("last bps = actual %d repair %d, want 38400/0", state.lastActualBps, state.lastRepairBps)
	}
}

func TestQoSEstimatorEmptyTickClearsCurrentRateStreams(t *testing.T) {
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
		t.Fatal("initial tick should publish raw rate state")
	}

	if !state.flush(now.Add(2*time.Second), time.Second) {
		t.Fatal("empty tick should still publish raw zero rate state")
	}
	if state.lastActual != 0 || state.lastRepair != 0 {
		t.Fatalf("last bytes = actual %d repair %d, want 0/0 after empty tick", state.lastActual, state.lastRepair)
	}
	if state.lastActualBps != 0 || state.lastRepairBps != 0 {
		t.Fatalf("last bps = actual %d repair %d, want 0/0 after empty tick", state.lastActualBps, state.lastRepairBps)
	}
}

func TestQoSEstimatorEmptyTicksDoNotSustainStaleRateGap(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	got = append(got, observeProfileRateTick(e, now, transport.KindUDP, transport.KindTCP, 1200, 1200, 4800, 1200)...)
	for i := 1; i < 5; i++ {
		got = append(got, e.Tick(now.Add(time.Duration(i+1)*time.Second))...)
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want no stale limited status from empty ticks", got)
	}
}

func TestQoSEstimatorMarksDataLegLimitedFromRateGap(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeProfileRateTick(e, at, transport.KindUDP, transport.KindTCP, 1200, 1200, 4800, 1200)...)
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
	if !got[0].primarySwitched {
		t.Fatalf("status = %+v, want primary switch marker", got[0])
	}
}

func TestQoSEstimatorHighGapCommitsAfterDecisionSamples(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: 3 * time.Second, Tick: time.Second}, nil)
	state := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)

	for i := 0; i < qosDecisionSamples-1; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if status, ok := e.driveHighGapState(state, qosLimitStateDataRate, 0.20, 100, at); ok {
			t.Fatalf("status at sample %d = %+v, want no commit before decision window is ready", i, status)
		}
	}

	got, ok := e.driveHighGapState(state, qosLimitStateDataRate, 0.20, 100, now.Add((qosDecisionSamples-1)*time.Second))
	if !ok || !got.UDPLimited || got.TCPLimited {
		t.Fatalf("status = %+v ok=%t, want UDP limited after decision window is ready", got, ok)
	}
}

func TestQoSEstimatorMarksDataLegLimitedWhenDataMissing(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)

	for i := 0; i < 2; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeProfileRateTick(e, at, transport.KindUDP, transport.KindTCP, 4800, 1200, 4800, 1200)...)
	}
	for i := 2; i < 8; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeProfileRateTick(e, at, transport.KindUDP, transport.KindTCP, 0, 1200, 4800, 1200)...)
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
	tcpShadow := e.direction(transport.KindTCP, transport.KindUDP, qosRoleShadow)

	got = append(got, observeProfileRateTick(e, now, transport.KindUDP, transport.KindTCP, 1200, 1200, 4800, 1200)...)
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

	for i := 1; i <= 4; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		got = append(got, observeProfileRateTick(e, at, transport.KindUDP, transport.KindTCP, 1200, 1200, 4800, 1200)...)
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

func TestQoSEstimatorShadowRoleDoesNotClearFromStaleRepairTick(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)

	status, ok := e.commitLimitState(udpData, qosLimitStateDataRate, true, 100, now, now)
	if !ok || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("initial status = %+v ok=%t, want UDP limited only", status, ok)
	}
	if e.currentPrimary != transport.KindTCP || e.currentRoleLocked() != qosRoleShadow {
		t.Fatalf("primary=%v role=%v, want TCP shadow after UDP limited", e.currentPrimary, e.currentRoleLocked())
	}

	var got []qosStatus
	got = append(got, observeProfileRateTick(e, now.Add(time.Second), transport.KindTCP, transport.KindUDP, 70_000_000, 17_500_000, 64_000_000, 16_000_000)...)
	got = append(got, observeProfileRateTick(e, now.Add(2*time.Second), transport.KindTCP, transport.KindUDP, 31_000_000, 7_800_000, 31_000_000, 7_750_000)...)
	got = append(got, observeProfileRateTick(e, now.Add(3*time.Second), transport.KindTCP, transport.KindUDP, 12_000_000, 1_200_000, 10_000_000, 2_500_000)...)

	for _, status := range got {
		if !status.UDPLimited {
			t.Fatalf("statuses = %+v, want UDP to remain limited when current repair tick is too small", got)
		}
	}
}

func TestQoSEstimatorPrimarySwitchResetsDirectionTickState(t *testing.T) {
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)
	udpData.lastActual = 1200
	udpData.lastRepair = 1200
	udpData.lastProfileData = 4800
	udpData.lastProfileRepair = 1200
	udpData.lastActualBps = 1200
	udpData.lastRepairBps = 1200

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

	if udpData.lastActual != 0 || udpData.lastRepair != 0 || udpData.lastProfileData != 0 || udpData.lastProfileRepair != 0 || udpData.lastActualBps != 0 || udpData.lastRepairBps != 0 {
		t.Fatalf("direction tick state = actual %d repair %d profile %d/%d bps %d/%d, want reset",
			udpData.lastActual, udpData.lastRepair, udpData.lastProfileData, udpData.lastProfileRepair, udpData.lastActualBps, udpData.lastRepairBps)
	}
}

func TestQoSEstimatorLateDataAfterLimitedDoesNotEraseCommittedState(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, nil)
	e.currentPrimary = transport.KindUDP
	udpData := e.direction(transport.KindUDP, transport.KindTCP, qosRoleData)

	if status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now); ok {
		t.Fatalf("first limited state = %+v, want pending only", status)
	}
	status, ok := e.driveLimitState(udpData, qosLimitStateDataRate, true, 100, now.Add(1100*time.Millisecond))
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
