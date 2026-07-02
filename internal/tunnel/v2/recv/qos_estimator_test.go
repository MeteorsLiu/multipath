package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func observeQoSRepairFrame(e *qosEstimator, group rxGroupKey, repairKind transport.Kind, repairBytes int, repairCount uint8, at time.Time) {
	e.observeRepairBytes(repairKind, repairBytes, at)
	e.observeRepairGroup(group, repairKind, repairCount)
}

func observeQoSGroup(e *qosEstimator, basePacketID uint32, at time.Time, originalBytes, expectedBytes uint64) []qosStatus {
	group := rxGroupKey{basePacketID: basePacketID, sourceSpan: 2}
	e.observeOriginalData(transport.KindUDP, int(originalBytes), at)
	observeQoSRepairFrame(e, group, transport.KindTCP, int(expectedBytes/2), 1, at)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    1,
		dataExpected:   2,
		expectedBytes:  expectedBytes,
		maxSourceBytes: expectedBytes / 2,
	}, at)
	return e.tick(at.Add(e.cfg.Tick))
}

func TestQoSEstimatorDataLegLimitedFromExpectedBytes(t *testing.T) {
	now := time.Unix(100, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	var statuses []qosStatus

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		statuses = append(statuses, observeQoSGroup(e, uint32(100+i*10), at, 100, 200)...)
	}

	if len(statuses) == 0 {
		t.Fatal("missing QoS status")
	}
	limited := statuses[len(statuses)-1]
	if !limited.UDPLimited || limited.TCPLimited {
		t.Fatalf("statuses = %+v, want final UDP limited only", statuses)
	}
	if !limited.primarySwitched || e.currentPrimary != transport.KindTCP {
		t.Fatalf("statuses = %+v primary=%v, want final switch to TCP", statuses, e.currentPrimary)
	}
}

func TestQoSEstimatorPrimaryHintAllowsTCPPrimarySamples(t *testing.T) {
	now := time.Unix(125, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.observePrimaryHint(qosPrimaryHint{primary: transport.KindTCP, at: now})

	group := rxGroupKey{basePacketID: 150, sourceSpan: 2}
	e.observeOriginalData(transport.KindTCP, 100, now)
	observeQoSRepairFrame(e, group, transport.KindUDP, 60, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    1,
		dataExpected:   2,
		expectedBytes:  200,
		maxSourceBytes: 100,
	}, now)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("primary = %v, want TCP", e.currentPrimary)
	}
	pending := e.tcpUDPData.pending
	if pending.originalDataBytes != 100 || pending.repairBytes != 60 || pending.expectedBytes != 200 {
		t.Fatalf("tcp/udp pending = %+v, want original=100 repair=60 expected=200", pending)
	}
}

func TestQoSEstimatorPrimaryHintUsesNewPrimaryAsDataRole(t *testing.T) {
	now := time.Unix(135, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true

	e.observePrimaryHint(qosPrimaryHint{primary: transport.KindTCP, at: now})

	e.mu.Lock()
	if e.currentPrimary != transport.KindTCP || e.currentRole != qosRoleData {
		t.Fatalf("primary=%v role=%s, want TCP data role", e.currentPrimary, qosRoleLabel(e.currentRole))
	}
	e.mu.Unlock()

	group := rxGroupKey{basePacketID: 160, sourceSpan: 2}
	e.observeOriginalData(transport.KindTCP, 100, now)
	observeQoSRepairFrame(e, group, transport.KindUDP, 60, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    1,
		dataExpected:   2,
		expectedBytes:  200,
		maxSourceBytes: 100,
	}, now)

	e.mu.Lock()
	defer e.mu.Unlock()
	dataPending := e.tcpUDPData.pending
	if dataPending.originalDataBytes != 100 || dataPending.repairBytes != 60 || dataPending.expectedBytes != 200 {
		t.Fatalf("tcp/udp data pending = %+v, want original=100 repair=60 expected=200", dataPending)
	}
	if repairPending := e.tcpUDPRepair.pending; repairPending != (qosPendingBytes{}) {
		t.Fatalf("tcp/udp repair pending = %+v, want empty", repairPending)
	}
}

func TestQoSEstimatorPrimaryHintWithBandwidthEvidenceMarksUDPLimited(t *testing.T) {
	now := time.Unix(145, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)

	statuses := e.observePrimaryHint(qosPrimaryHint{
		primary:         transport.KindTCP,
		udpLimited:      true,
		udpDeliveredBps: 20_000_000,
		tcpDeliveredBps: 80_000_000,
		at:              now,
	})

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one limited status", statuses)
	}
	status := statuses[0]
	if !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP limited only", status)
	}
	if status.UDPDeliveredBps != 20_000_000 || status.TCPDeliveredBps != 80_000_000 {
		t.Fatalf("status delivered bps udp=%d tcp=%d, want udp=20000000 tcp=80000000", status.UDPDeliveredBps, status.TCPDeliveredBps)
	}
	if !status.primarySwitched || e.currentPrimary != transport.KindTCP || e.currentRole != qosRoleRepair {
		t.Fatalf("status=%+v primary=%v role=%s, want TCP repair role after UDP limited evidence", status, e.currentPrimary, qosRoleLabel(e.currentRole))
	}
}

func TestQoSEstimatorDataLegRateGapCountsLateData(t *testing.T) {
	now := time.Unix(150, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		packetID := uint32(200 + i)
		group := rxGroupKey{basePacketID: uint32(200 + i*10), sourceSpan: 2}

		e.observeOriginalData(transport.KindUDP, 100, at)
		observeQoSRepairFrame(e, group, transport.KindTCP, 60, 1, at)
		e.observeRecoveredData(packetID)
		e.observeLateData(packetID, transport.KindUDP, 20, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    2,
			dataExpected:   2,
			expectedBytes:  120,
			maxSourceBytes: 60,
		}, at)
		if statuses := e.tick(at.Add(time.Second)); len(statuses) != 0 {
			t.Fatalf("statuses after tick %d = %+v, want no QoS status", i, statuses)
		}
	}

	status := e.snapshotStatus()
	if status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want no limited transport", status)
	}
	if e.currentPrimary != transport.KindUDP {
		t.Fatalf("primary = %v, want UDP", e.currentPrimary)
	}
}

func TestQoSEstimatorExpiredGroupUpdatesFECHealthOnlyOnTick(t *testing.T) {
	now := time.Unix(200, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 500, sourceSpan: 4}

	observeQoSRepairFrame(e, group, transport.KindTCP, 1200, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:        group,
		dataExpected: 4,
		expired:      true,
	}, now)

	if got := e.snapshotStatus().RepairCount; got != 1 {
		t.Fatalf("repair count before tick = %d, want 1", got)
	}
	statuses := e.tick(now.Add(time.Second))
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want repair-count status", statuses)
	}
	if statuses[0].RepairCount != 4 {
		t.Fatalf("repair count status = %+v, want 4", statuses[0])
	}
	if statuses[0].UDPLimited || statuses[0].TCPLimited {
		t.Fatalf("status = %+v, want FEC health without QoS limited state", statuses[0])
	}
}

func TestQoSEstimatorFECHealthUpdatesLossEMAOnGroupArrival(t *testing.T) {
	now := time.Unix(225, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 550, sourceSpan: 4}

	observeQoSRepairFrame(e, group, transport.KindTCP, 1200, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:        group,
		dataExpected: 4,
		expired:      true,
	}, now)

	e.mu.Lock()
	health := e.currentDirectionLocked().fecHealth
	e.mu.Unlock()

	if !health.lossInitialized || health.lossRatio != 1 {
		t.Fatalf("FEC health after group = %+v, want initialized lossRatio=1 before tick", health)
	}
	if health.repairCount != 1 {
		t.Fatalf("repair count before tick = %d, want unchanged 1", health.repairCount)
	}
}

func TestQoSEstimatorFECHealthKeepsLateSeparateFromLoss(t *testing.T) {
	now := time.Unix(240, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 560, sourceSpan: 1}

	observeQoSRepairFrame(e, group, transport.KindTCP, 100, 1, now)
	e.observeRecoveredData(560)
	e.observeLateData(560, transport.KindUDP, 100, now)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    1,
		dataExpected:   1,
		expectedBytes:  100,
		maxSourceBytes: 100,
	}, now)

	statuses := e.tick(now.Add(time.Second))
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want repair-count status", statuses)
	}
	if statuses[0].RepairCount != 3 {
		t.Fatalf("repair count status = %+v, want late-health repair count 3", statuses[0])
	}

	e.mu.Lock()
	health := e.currentDirectionLocked().fecHealth
	e.mu.Unlock()
	if health.lossRatio != 0 {
		t.Fatalf("lossRatio = %v, want 0", health.lossRatio)
	}
	if !health.lateInitialized || health.lateRatio != 1 {
		t.Fatalf("late health = %+v, want initialized lateRatio=1", health)
	}
}

func TestQoSEstimatorKeepsRepairCountUntilPrimarySwitch(t *testing.T) {
	now := time.Unix(250, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 600, sourceSpan: 4}

	observeQoSRepairFrame(e, group, transport.KindTCP, 1200, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:        group,
		dataExpected: 4,
		expired:      true,
	}, now)
	statuses := e.tick(now.Add(time.Second))
	if len(statuses) != 1 || statuses[0].RepairCount != 4 {
		t.Fatalf("statuses = %+v, want repair count 4", statuses)
	}
	if got := e.snapshotStatus().RepairCount; got != 4 {
		t.Fatalf("repair count after health tick = %d, want 4", got)
	}

	if statuses := e.tick(now.Add(2 * time.Second)); len(statuses) != 0 {
		t.Fatalf("idle statuses = %+v, want none", statuses)
	}
	if got := e.snapshotStatus().RepairCount; got != 4 {
		t.Fatalf("repair count after idle tick = %d, want 4", got)
	}

	var switched []qosStatus
	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i+3) * time.Second)
		group := rxGroupKey{basePacketID: uint32(700 + i*10), sourceSpan: 2}
		e.observeOriginalData(transport.KindUDP, 100, at)
		observeQoSRepairFrame(e, group, transport.KindTCP, 100, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    2,
			dataExpected:   2,
			expectedBytes:  200,
			maxSourceBytes: 100,
		}, at)
		switched = append(switched, e.tick(at.Add(time.Second))...)
	}
	if len(switched) == 0 {
		t.Fatal("missing primary switch status")
	}
	last := switched[len(switched)-1]
	if !last.primarySwitched || e.currentPrimary != transport.KindTCP {
		t.Fatalf("statuses = %+v primary=%v, want switch to TCP", switched, e.currentPrimary)
	}
	if last.RepairCount != 1 {
		t.Fatalf("switch status repair count = %d, want 1", last.RepairCount)
	}
	if got := e.snapshotStatus().RepairCount; got != last.RepairCount {
		t.Fatalf("snapshot repair count = %d, status repair count = %d", got, last.RepairCount)
	}
}

func TestQoSEstimatorResetsSustainedMaxRepairCount(t *testing.T) {
	now := time.Unix(255, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 650, sourceSpan: 4}
	at := now.Add(-time.Second)

	observeQoSRepairFrame(e, group, transport.KindTCP, 1200, 1, at)
	e.observeGroupDone(rxGroupDone{
		group:        group,
		dataExpected: 4,
		expired:      true,
	}, at)

	statuses := e.tick(now)
	if len(statuses) != 1 || statuses[0].RepairCount != 4 {
		t.Fatalf("statuses = %+v, want repair count 4", statuses)
	}

	if statuses := e.tick(now.Add(74 * time.Second)); len(statuses) != 0 {
		t.Fatalf("statuses before dwell expiry = %+v, want none", statuses)
	}
	if got := e.snapshotStatus().RepairCount; got != 4 {
		t.Fatalf("repair count before dwell expiry = %d, want 4", got)
	}

	statuses = e.tick(now.Add(75 * time.Second))
	if len(statuses) != 1 {
		t.Fatalf("statuses after dwell expiry = %+v, want reset status", statuses)
	}
	if statuses[0].RepairCount != 1 {
		t.Fatalf("repair count after dwell expiry = %d, want 1", statuses[0].RepairCount)
	}
	if statuses[0].UDPLimited || statuses[0].TCPLimited {
		t.Fatalf("status after dwell expiry = %+v, want FEC reset without QoS limited state", statuses[0])
	}
}

func TestQoSEstimatorDoesNotDwellResetRepairCountThree(t *testing.T) {
	now := time.Unix(260, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 660, sourceSpan: 1}
	at := now.Add(-time.Second)

	observeQoSRepairFrame(e, group, transport.KindTCP, 100, 1, at)
	e.observeRecoveredData(660)
	e.observeLateData(660, transport.KindUDP, 100, at)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    1,
		dataExpected:   1,
		expectedBytes:  100,
		maxSourceBytes: 100,
	}, at)

	statuses := e.tick(now)
	if len(statuses) != 1 || statuses[0].RepairCount != 3 {
		t.Fatalf("statuses = %+v, want repair count 3", statuses)
	}

	if statuses := e.tick(now.Add(90 * time.Second)); len(statuses) != 0 {
		t.Fatalf("statuses after repair count 3 dwell = %+v, want none", statuses)
	}
	if got := e.snapshotStatus().RepairCount; got != 3 {
		t.Fatalf("repair count after repair count 3 dwell = %d, want 3", got)
	}
}

func TestQoSEstimatorRepairLegLimitedUsesMaxSourceRatioAndRepairCount(t *testing.T) {
	now := time.Unix(275, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1000 + i*10), sourceSpan: 4}
		observeQoSRepairFrame(e, group, transport.KindUDP, 1500, 2, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    4,
			dataExpected:   4,
			expectedBytes:  1110,
			maxSourceBytes: 1000,
		}, at)
		e.tick(at.Add(time.Second))
	}

	limited := e.snapshotStatus()
	if !limited.UDPLimited || limited.TCPLimited {
		t.Fatalf("status = %+v, want final UDP repair leg limited only", limited)
	}
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("primary = %v, want TCP", e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearRequiresRepairDelivery(t *testing.T) {
	now := time.Unix(285, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1100 + i*10), sourceSpan: 4}
		observeQoSRepairFrame(e, group, transport.KindUDP, 700, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    4,
			dataExpected:   4,
			expectedBytes:  4000,
			maxSourceBytes: 1000,
		}, at)
		e.tick(at.Add(time.Second))
	}

	status := e.snapshotStatus()
	if !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP still limited when repair delivery is below expected repair", status)
	}
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("primary = %v, want TCP", e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearWhenDeliveryAndLoadRecover(t *testing.T) {
	now := time.Unix(290, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	var statuses []qosStatus
	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1200 + i*10), sourceSpan: 1}
		observeQoSRepairFrame(e, group, transport.KindUDP, 4000, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    1,
			dataExpected:   1,
			expectedBytes:  4000,
			maxSourceBytes: 4000,
		}, at)
		statuses = append(statuses, e.tick(at.Add(time.Second))...)
	}

	if len(statuses) == 0 {
		t.Fatal("missing clear status")
	}
	last := statuses[len(statuses)-1]
	if last.UDPLimited || last.TCPLimited {
		t.Fatalf("statuses = %+v, want clear state", statuses)
	}
	if !last.primarySwitched || e.currentPrimary != transport.KindUDP {
		t.Fatalf("statuses = %+v primary=%v, want switch back to UDP", statuses, e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearRequiresNearDataLoad(t *testing.T) {
	now := time.Unix(295, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1300 + i*10), sourceSpan: 4}
		observeQoSRepairFrame(e, group, transport.KindUDP, 1000, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    4,
			dataExpected:   4,
			expectedBytes:  4000,
			maxSourceBytes: 1000,
		}, at)
		e.tick(at.Add(time.Second))
	}

	status := e.snapshotStatus()
	if !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP still limited after default 4+1 repair load", status)
	}
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("primary = %v, want TCP", e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearAllowsCurrentFullDataLoad(t *testing.T) {
	now := time.Unix(296, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	var statuses []qosStatus
	for i, repairBytes := range []int{1000, 1000, 4000} {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1325 + i*10), sourceSpan: 4}
		observeQoSRepairFrame(e, group, transport.KindUDP, repairBytes, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    4,
			dataExpected:   4,
			expectedBytes:  4000,
			maxSourceBytes: 1000,
		}, at)
		statuses = append(statuses, e.tick(at.Add(time.Second))...)
	}

	if len(statuses) == 0 {
		t.Fatal("missing clear status")
	}
	last := statuses[len(statuses)-1]
	if last.UDPLimited || last.TCPLimited {
		t.Fatalf("statuses = %+v, want clear state after current full repair load", statuses)
	}
	if !last.primarySwitched || e.currentPrimary != transport.KindUDP {
		t.Fatalf("statuses = %+v primary=%v, want switch back to UDP", statuses, e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearAllowsNearDataLoad(t *testing.T) {
	now := time.Unix(297, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.udpTCPData.dataLimited = true
	e.currentPrimary = transport.KindTCP
	e.currentRole = qosRoleRepair

	var statuses []qosStatus
	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1350 + i*10), sourceSpan: 4}
		observeQoSRepairFrame(e, group, transport.KindUDP, 4000, 4, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    4,
			dataExpected:   4,
			expectedBytes:  4000,
			maxSourceBytes: 1000,
		}, at)
		statuses = append(statuses, e.tick(at.Add(time.Second))...)
	}

	if len(statuses) == 0 {
		t.Fatal("missing clear status")
	}
	last := statuses[len(statuses)-1]
	if last.UDPLimited || last.TCPLimited {
		t.Fatalf("statuses = %+v, want clear state", statuses)
	}
	if !last.primarySwitched || e.currentPrimary != transport.KindUDP {
		t.Fatalf("statuses = %+v primary=%v, want switch back to UDP", statuses, e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearRequiresCapLoad(t *testing.T) {
	now := time.Unix(298, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.observePrimaryHint(qosPrimaryHint{
		primary:    transport.KindTCP,
		udpLimited: true,
		capBps:     200_000_000,
		at:         now,
	})

	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1375 + i*10), sourceSpan: 1}
		observeQoSRepairFrame(e, group, transport.KindUDP, 4000, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    1,
			dataExpected:   1,
			expectedBytes:  4000,
			maxSourceBytes: 4000,
		}, at)
		e.tick(at.Add(time.Second))
	}

	status := e.snapshotStatus()
	if !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP still limited when repair load is below cap", status)
	}
	if e.currentPrimary != transport.KindTCP {
		t.Fatalf("primary = %v, want TCP", e.currentPrimary)
	}
}

func TestQoSEstimatorRepairLegClearAllowsEightyPercentCapLoad(t *testing.T) {
	now := time.Unix(299, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	e.observePrimaryHint(qosPrimaryHint{
		primary:    transport.KindTCP,
		udpLimited: true,
		capBps:     200_000_000,
		at:         now,
	})

	var statuses []qosStatus
	for i := 0; i < qosDecisionSamples; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		group := rxGroupKey{basePacketID: uint32(1390 + i*10), sourceSpan: 1}
		observeQoSRepairFrame(e, group, transport.KindUDP, 20_100_000, 1, at)
		e.observeGroupDone(rxGroupDone{
			group:          group,
			dataArrived:    1,
			dataExpected:   1,
			expectedBytes:  4000,
			maxSourceBytes: 4000,
		}, at)
		statuses = append(statuses, e.tick(at.Add(time.Second))...)
	}

	if len(statuses) == 0 {
		t.Fatal("missing clear status")
	}
	last := statuses[len(statuses)-1]
	if last.UDPLimited || last.TCPLimited {
		t.Fatalf("statuses = %+v, want clear state at 80%% cap repair load", statuses)
	}
	if !last.primarySwitched || e.currentPrimary != transport.KindUDP {
		t.Fatalf("statuses = %+v primary=%v, want switch back to UDP", statuses, e.currentPrimary)
	}
}

func TestQoSEstimatorLateDataStaysSeparateFromOriginalData(t *testing.T) {
	now := time.Unix(300, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 700, sourceSpan: 1}

	observeQoSRepairFrame(e, group, transport.KindTCP, 100, 1, now)
	e.observeRecoveredData(700)
	e.observeLateData(700, transport.KindUDP, 100, now)

	e.mu.Lock()
	state := e.currentDirectionLocked()
	pending := state.pending
	e.mu.Unlock()

	if pending.originalDataBytes != 0 {
		t.Fatalf("originalDataBytes = %d, want 0 for late DATA", pending.originalDataBytes)
	}
	if pending.lateDataBytes != 100 {
		t.Fatalf("lateDataBytes = %d, want 100", pending.lateDataBytes)
	}
}

func TestQoSEstimatorDuplicateOriginalIsNotLateData(t *testing.T) {
	now := time.Unix(350, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)

	e.observeLateData(800, transport.KindUDP, 100, now)

	e.mu.Lock()
	pending := e.currentDirectionLocked().pending
	e.mu.Unlock()

	if pending.lateDataBytes != 0 {
		t.Fatalf("lateDataBytes = %d, want 0 for duplicate original DATA", pending.lateDataBytes)
	}
}

func TestQoSEstimatorGroupDoneAddsOnlyFourPendingCounters(t *testing.T) {
	now := time.Unix(400, 0)
	e := newQoSEstimator(qosConfig{Tick: time.Second}, nil)
	group := rxGroupKey{basePacketID: 900, sourceSpan: 2}

	observeQoSRepairFrame(e, group, transport.KindTCP, 100, 1, now)
	e.observeGroupDone(rxGroupDone{
		group:          group,
		dataArrived:    2,
		dataExpected:   2,
		expectedBytes:  200,
		maxSourceBytes: 100,
	}, now)

	e.mu.Lock()
	pending := e.currentDirectionLocked().pending
	e.mu.Unlock()

	if pending.expectedBytes != 200 {
		t.Fatalf("expectedBytes = %d, want 200", pending.expectedBytes)
	}
	if pending.repairBytes != 100 {
		t.Fatalf("repairBytes = %d, want 100", pending.repairBytes)
	}
	if pending.lateDataBytes != 0 {
		t.Fatalf("lateDataBytes = %d, want 0", pending.lateDataBytes)
	}
}
