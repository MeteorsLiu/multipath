package recv

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestQoSEstimatorStaysSilentBelowSampleFloor(t *testing.T) {
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 100}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:           time.Unix(0, 0),
		DataKind:     transport.KindUDP,
		DataArrived:  20,
		DataExpected: 20,
		DataBytes:    1200,
	})

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none below sample floor", got)
	}
}

func TestQoSEstimatorInfersDataLossFromDifferentialSample(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 100, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:           now,
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  40,
		DataExpected: 160,
		DataBytes:    40 * 1200,
	})
	e.Observe(qosSample{
		At:           now.Add(2 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  40,
		DataExpected: 160,
		DataBytes:    40 * 1200,
	})

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one", got)
	}
	if got[0].Kind != transport.KindUDP || got[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
}

func TestQoSEstimatorComputesActualRateFromRecoveredBytes(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:             now,
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    2,
		DataExpected:   4,
		DataBytes:      2400,
		RecoveredBytes: 2400,
	})
	e.Observe(qosSample{
		At:             now.Add(2 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    2,
		DataExpected:   4,
		DataBytes:      2400,
		RecoveredBytes: 2400,
	})

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one", got)
	}
	if got[0].DeliveredBps == 0 {
		t.Fatalf("DeliveredBps = 0, want actual rate from arrived + recovered bytes")
	}
}

func TestQoSEstimatorDetectsBackloggedDataLegFromLag(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 100, Sustain: time.Second, LagSlack: 300 * time.Millisecond}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:           now,
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  160,
		DataExpected: 160,
		DataBytes:    160 * 1200,
		Lag:          50 * time.Millisecond,
	})
	e.Observe(qosSample{
		At:           now.Add(1500 * time.Millisecond),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  160,
		DataExpected: 160,
		DataBytes:    160 * 1200,
		Lag:          50 * time.Millisecond,
	})
	if len(got) != 0 {
		t.Fatalf("baseline statuses = %+v, want none", got)
	}

	e.Observe(qosSample{
		At:           now.Add(2 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  160,
		DataExpected: 160,
		DataBytes:    160 * 1200,
		Lag:          900 * time.Millisecond,
	})
	e.Observe(qosSample{
		At:           now.Add(4 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  160,
		DataExpected: 160,
		DataBytes:    160 * 1200,
		Lag:          900 * time.Millisecond,
	})

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one", got)
	}
	if got[0].Kind != transport.KindTCP || got[0].Reason != protocol.LinkStatusReasonBacklogged {
		t.Fatalf("status = %+v, want TCP backlogged", got[0])
	}
}

func TestQoSEstimatorRefreshesSustainedStatus(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second, Refresh: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:           now,
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  1,
		DataExpected: 4,
		DataBytes:    1200,
	})
	e.Observe(qosSample{
		At:           now.Add(2 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  1,
		DataExpected: 4,
		DataBytes:    1200,
	})
	if len(got) != 1 {
		t.Fatalf("first evaluate statuses = %+v, want one", got)
	}
	e.Observe(qosSample{
		At:           now.Add(2250 * time.Millisecond),
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  1,
		DataExpected: 4,
		DataBytes:    1200,
	})
	if len(got) != 1 {
		t.Fatalf("pre-refresh statuses = %+v, want no duplicate", got)
	}
	e.Observe(qosSample{
		At:           now.Add(3 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  1,
		DataExpected: 4,
		DataBytes:    1200,
	})
	if len(got) != 2 {
		t.Fatalf("refresh statuses = %+v, want refreshed status", got)
	}
}

func TestLaneArrivalStatsBuildsDeltaQoSSample(t *testing.T) {
	now := time.Unix(0, 0)
	stats := &laneArrivalStats{}
	stats.recordArrival(transport.KindUDP, catData, 1000)
	stats.recordArrival(transport.KindTCP, catRepair, 1000)
	stats.recordRecovered(transport.KindUDP, 500)

	if sample, ok := stats.qosDelta(transport.KindUDP, now); ok {
		t.Fatalf("first sample = %+v, want cursor initialization only", sample)
	}
	if sample, ok := stats.qosDelta(transport.KindUDP, now.Add(time.Second)); ok {
		t.Fatalf("early sample = %+v, want no sample before interval", sample)
	}

	stats.recordArrival(transport.KindUDP, catData, 1200)
	stats.recordArrival(transport.KindTCP, catRepair, 1200)
	stats.recordRecovered(transport.KindUDP, 700)
	sample, ok := stats.qosDelta(transport.KindUDP, now.Add(3*time.Second))
	if !ok {
		t.Fatal("missing delta sample")
	}
	if sample.DataArrived != 1 || sample.DataExpected != maxFECSourceSpan {
		t.Fatalf("sample counts = got arrived %d expected %d", sample.DataArrived, sample.DataExpected)
	}
	if sample.DataBytes != 1200 || sample.RepairBytes != 1200 || sample.RecoveredBytes != 700 {
		t.Fatalf("sample bytes = %+v, want data=1200 repair=1200 recovered=700", sample)
	}
	if sample.Duration != 3*time.Second {
		t.Fatalf("duration = %s, want 3s", sample.Duration)
	}
}
