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

func TestQoSEstimatorAggregatesSmallFECSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 4; i++ {
		e.Observe(qosSample{
			At:             now.Add(time.Duration(i) * 200 * time.Millisecond),
			Duration:       200 * time.Millisecond,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    0,
			DataExpected:   1,
			RecoveredBytes: 84,
		})
	}
	if len(got) != 0 {
		t.Fatalf("first sustained window statuses = %+v, want none", got)
	}

	for i := 0; i < 4; i++ {
		e.Observe(qosSample{
			At:             now.Add(2*time.Second + time.Duration(i)*200*time.Millisecond),
			Duration:       200 * time.Millisecond,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    0,
			DataExpected:   1,
			RecoveredBytes: 84,
		})
	}
	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one after aggregated sustained loss", got)
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
