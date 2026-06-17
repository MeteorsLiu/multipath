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

func TestQoSEstimatorIgnoresSameKindSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 2; i++ {
		e.Observe(qosSample{
			At:             now.Add(time.Duration(i) * 2 * time.Second),
			Duration:       time.Second,
			DataKind:       transport.KindTCP,
			RepairKind:     transport.KindTCP,
			DataArrived:    1,
			DataExpected:   4,
			DataBytes:      1200,
			RecoveredBytes: 3 * 1200,
			RepairBytes:    1200,
		})
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none for same-kind DATA/REPAIR samples", got)
	}
}

func TestQoSEstimatorInfersRateGapFromDifferentialSample(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 100, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:             now,
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    40,
		DataExpected:   160,
		DataBytes:      40 * 1200,
		RecoveredBytes: 120 * 1200,
		RepairBytes:    40 * 1200,
	})
	e.Observe(qosSample{
		At:             now.Add(2 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    40,
		DataExpected:   160,
		DataBytes:      40 * 1200,
		RecoveredBytes: 120 * 1200,
		RepairBytes:    40 * 1200,
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
			RepairBytes:    84,
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
			RepairBytes:    84,
		})
	}
	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one after aggregated sustained rate gap", got)
	}
	if got[0].Kind != transport.KindUDP || got[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
}

func TestQoSEstimatorSustainsRateGapAcrossCleanSmallSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second, Refresh: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 80; i++ {
		arrived := uint64(1)
		recovered := uint64(0)
		if i%5 == 0 {
			arrived = 0
			recovered = 84
		}
		e.Observe(qosSample{
			At:             now.Add(time.Duration(i) * 100 * time.Millisecond),
			Duration:       100 * time.Millisecond,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    arrived,
			DataExpected:   1,
			DataBytes:      arrived * 84,
			RecoveredBytes: recovered,
			RepairBytes:    84,
		})
	}

	if len(got) == 0 {
		t.Fatal("statuses = none, want UDP limited despite clean samples inside sustained 20% rate gap")
	}
	if got[0].Kind != transport.KindUDP || got[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
}

func TestQoSEstimatorEMAClearsAfterSustainedCleanSamples(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second, Refresh: time.Second}, nil)

	var gap float64
	for i := 0; i < 100; i++ {
		arrived := uint64(1)
		recovered := uint64(0)
		if i < 40 && i%5 == 0 {
			arrived = 0
			recovered = 84
		}
		estimate, ok := e.updateEstimate(qosSample{
			At:             now.Add(time.Duration(i) * 100 * time.Millisecond),
			Duration:       100 * time.Millisecond,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    arrived,
			DataExpected:   1,
			DataBytes:      arrived * 84,
			RecoveredBytes: recovered,
			RepairBytes:    84,
		})
		if ok {
			gap = estimate.RateGapRatio
		}
	}
	if gap >= qosRateGapExit {
		t.Fatalf("EMA rate gap after clean samples = %.3f, want below exit %.3f", gap, qosRateGapExit)
	}
}

func TestQoSEstimatorReportsActualDeliveredRate(t *testing.T) {
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
		RepairBytes:    1200,
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
		RepairBytes:    1200,
	})

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one", got)
	}
	want := uint32(2400 * 8)
	if got[0].DeliveredBps != want {
		t.Fatalf("DeliveredBps = %d, want actual DATA rate %d", got[0].DeliveredBps, want)
	}
}

func TestQoSEstimatorUsesByteRateGapNotPacketLoss(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1}, nil)

	estimate, ok := e.updateEstimate(qosSample{
		At:             now,
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   2,
		DataBytes:      951,
		RecoveredBytes: 49,
		RepairBytes:    500,
	})
	if !ok {
		t.Fatal("estimate = none, want one")
	}
	if estimate.RateGapRatio >= qosRateGapEnter {
		t.Fatalf("rate gap = %.3f, want below enter threshold despite 50%% packet loss", estimate.RateGapRatio)
	}
	if estimate.ActualBps != 951*8 || estimate.ExpectedBps != 1000*8 {
		t.Fatalf("rates actual=%d expected=%d, want byte-derived rates", estimate.ActualBps, estimate.ExpectedBps)
	}
}

func TestQoSEstimatorFitsIdealFECShadowRate(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1}, nil)

	var estimate qosEstimate
	for i := 0; i < 8; i++ {
		var ok bool
		estimate, ok = e.updateEstimate(qosSample{
			At:           now.Add(time.Duration(i) * time.Second),
			Duration:     time.Second,
			DataKind:     transport.KindUDP,
			RepairKind:   transport.KindTCP,
			DataArrived:  4,
			DataExpected: 4,
			DataBytes:    4 * 1200,
			RepairBytes:  1200,
		})
		if !ok {
			t.Fatal("estimate = none, want one")
		}
	}

	want := uint32(4 * 1200 * 8)
	if estimate.ShadowBps != want {
		t.Fatalf("ShadowBps = %d, want ideal FEC equivalent rate %d", estimate.ShadowBps, want)
	}
}

func TestQoSEstimatorRequiresShadowAdvantage(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 2; i++ {
		e.Observe(qosSample{
			At:             now.Add(time.Duration(i) * 2 * time.Second),
			Duration:       time.Second,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    3,
			DataExpected:   4,
			DataBytes:      3 * 1200,
			RecoveredBytes: 1200,
			RepairBytes:    300,
		})
	}

	if len(got) != 0 {
		t.Fatalf("statuses = %+v, want none when shadow estimate is weaker than DATA actual rate", got)
	}
}

func TestQoSEstimatorInfersShadowLegLimited(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 2; i++ {
		e.Observe(qosSample{
			At:           now.Add(time.Duration(i) * 2 * time.Second),
			Duration:     time.Second,
			DataKind:     transport.KindTCP,
			RepairKind:   transport.KindUDP,
			DataArrived:  4,
			DataExpected: 4,
			DataBytes:    4 * 1200,
			RepairBytes:  300,
		})
	}

	if len(got) != 1 {
		t.Fatalf("statuses = %+v, want one", got)
	}
	if got[0].Kind != transport.KindUDP || got[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited from weak shadow leg", got[0])
	}
	want := uint32(300 * 4 * 8)
	if got[0].DeliveredBps != want {
		t.Fatalf("DeliveredBps = %d, want shadow equivalent rate %d", got[0].DeliveredBps, want)
	}
}

func TestQoSEstimatorShadowLimitedUsesRateGapThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, nil)
	sample := qosEstimate{
		At:           now,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		SampleTotal:  4,
		RateGapRatio: 0,
		ActualBps:    100_000,
		ShadowBps:    95_000,
	}

	if status, ok := e.evaluateShadowLimited(sample, now.Add(2*time.Second)); ok {
		t.Fatalf("status = %+v, want no shadow limited below rate gap threshold", status)
	}

	sample.ShadowBps = 80_000
	if _, ok := e.evaluateShadowLimited(sample, now.Add(4*time.Second)); ok {
		t.Fatal("first over-threshold shadow sample should only start sustain timer")
	}
	status, ok := e.evaluateShadowLimited(sample, now.Add(6*time.Second))
	if !ok {
		t.Fatal("missing shadow limited after sustained deficit")
	}
	if status.Kind != transport.KindUDP || status.Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited", status)
	}
}

func TestQoSEstimatorKeepsIndependentDirectionState(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1}, nil)

	if _, ok := e.updateEstimate(qosSample{
		At:           now,
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1200,
	}); !ok {
		t.Fatal("first UDP/TCP estimate = none, want one")
	}
	if _, ok := e.updateEstimate(qosSample{
		At:           now.Add(time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  300,
	}); !ok {
		t.Fatal("TCP/UDP estimate = none, want one")
	}
	if _, ok := e.updateEstimate(qosSample{
		At:           now.Add(2 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1200,
	}); !ok {
		t.Fatal("second UDP/TCP estimate = none, want one")
	}

	udpTCP := e.ema[qosDirectionKey{dataKind: transport.KindUDP, repairKind: transport.KindTCP}]
	tcpUDP := e.ema[qosDirectionKey{dataKind: transport.KindTCP, repairKind: transport.KindUDP}]
	if udpTCP == nil || tcpUDP == nil {
		t.Fatalf("direction states udp/tcp=%v tcp/udp=%v, want both", udpTCP, tcpUDP)
	}
	if udpTCP.sampleTotal != 8 {
		t.Fatalf("UDP/TCP sampleTotal = %d, want 8", udpTCP.sampleTotal)
	}
	if tcpUDP.sampleTotal != 4 {
		t.Fatalf("TCP/UDP sampleTotal = %d, want 4", tcpUDP.sampleTotal)
	}
}

func TestQoSEstimatorPIDAutoGateFreezesOnShadowDeficit(t *testing.T) {
	state := qosEMAState{dataKind: transport.KindTCP, repairKind: transport.KindUDP}
	for i := 0; i < qosShadowPIDWarmupSamples; i++ {
		state.observe(qosSample{
			Duration:     time.Second,
			DataKind:     transport.KindTCP,
			RepairKind:   transport.KindUDP,
			DataArrived:  4,
			DataExpected: 4,
			DataBytes:    4 * 1200,
			RepairBytes:  1200,
		})
	}

	before := state.pidCorrection
	state.sampleCount++
	state.actualBpsEMA = 100_000
	state.expectedBpsEMA = 100_000
	state.repairBpsEMA = 10_000
	state.profileDataEMA = 40_000
	state.profileRepairEMA = 20_000
	state.trainPID()

	if state.pidCorrection != before {
		t.Fatalf("pidCorrection = %.3f, want frozen correction %.3f on shadow deficit", state.pidCorrection, before)
	}
}

func TestQoSEstimatorPIDTrainsOnHealthyResidual(t *testing.T) {
	state := qosEMAState{dataKind: transport.KindUDP, repairKind: transport.KindTCP}
	state.observe(qosSample{
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1200,
	})
	state.observe(qosSample{
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1150,
	})

	if state.pidCorrection <= 0 {
		t.Fatalf("pidCorrection = %.3f, want positive correction after healthy residual", state.pidCorrection)
	}
}

func TestQoSEstimatorClearsWhenShadowAdvantageDisappears(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Refresh: time.Hour}, func(status qosStatus) {
		got = append(got, status)
	})

	for i := 0; i < 2; i++ {
		e.Observe(qosSample{
			At:             now.Add(time.Duration(i) * 2 * time.Second),
			Duration:       time.Second,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    1,
			DataExpected:   4,
			DataBytes:      1200,
			RecoveredBytes: 3 * 1200,
			RepairBytes:    1200,
		})
	}
	if len(got) != 1 {
		t.Fatalf("active statuses = %+v, want one", got)
	}

	for i := 0; i < 30; i++ {
		e.Observe(qosSample{
			At:             now.Add(time.Duration(3+i) * time.Second),
			Duration:       time.Second,
			DataKind:       transport.KindUDP,
			RepairKind:     transport.KindTCP,
			DataArrived:    3,
			DataExpected:   4,
			DataBytes:      3 * 1200,
			RecoveredBytes: 1200,
			RepairBytes:    300,
		})
	}
	e.Observe(qosSample{
		At:           now.Add(40 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1200,
	})
	if len(got) != 1 {
		t.Fatalf("statuses after shadow clear = %+v, want no refreshed limited status", got)
	}
}

func TestQoSEstimatorRefreshesSustainedStatus(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second, Refresh: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:             now,
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	e.Observe(qosSample{
		At:             now.Add(2 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	if len(got) != 1 {
		t.Fatalf("first evaluate statuses = %+v, want one", got)
	}
	e.Observe(qosSample{
		At:             now.Add(2250 * time.Millisecond),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	if len(got) != 1 {
		t.Fatalf("pre-refresh statuses = %+v, want no duplicate", got)
	}
	e.Observe(qosSample{
		At:             now.Add(3 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	if len(got) != 2 {
		t.Fatalf("refresh statuses = %+v, want refreshed status", got)
	}
}

func TestQoSEstimatorRefreshesActiveStatusFromOtherLegSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second, Refresh: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	e.Observe(qosSample{
		At:             now,
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	e.Observe(qosSample{
		At:             now.Add(3 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    1,
		DataExpected:   4,
		DataBytes:      1200,
		RecoveredBytes: 3 * 1200,
		RepairBytes:    1200,
	})
	if len(got) != 1 {
		t.Fatalf("active statuses = %+v, want one", got)
	}

	e.Observe(qosSample{
		At:           now.Add(4 * time.Second),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4800,
	})
	if len(got) != 2 {
		t.Fatalf("refresh from other leg statuses = %+v, want two", got)
	}
	if got[1].Kind != transport.KindUDP || got[1].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("refresh status = %+v, want UDP limited", got[1])
	}

	e.Observe(qosSample{
		At:           now.Add(7*time.Second + time.Millisecond),
		Duration:     time.Second,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4800,
	})
	if len(got) != 2 {
		t.Fatalf("stale active status refreshed = %+v, want no third status", got)
	}
}
