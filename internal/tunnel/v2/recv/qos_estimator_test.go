package recv

import (
	"testing"
	"time"

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
	if !got[0].UDPLimited || got[0].TCPLimited {
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
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
}

func TestQoSEstimatorSustainsRateGapAcrossCleanSmallSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second}, func(status qosStatus) {
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
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited", got[0])
	}
}

func TestQoSEstimatorEMAClearsAfterSustainedCleanSamples(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second}, nil)

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
	if got[0].UDPDeliveredBps != want {
		t.Fatalf("UDPDeliveredBps = %d, want actual DATA rate %d", got[0].UDPDeliveredBps, want)
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
			At:           now.Add(time.Duration(i) * time.Second),
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
	if !got[0].UDPLimited || got[0].TCPLimited {
		t.Fatalf("status = %+v, want UDP limited from weak shadow leg", got[0])
	}
	want := uint32(300 * 4 * 8)
	if got[0].UDPDeliveredBps != want {
		t.Fatalf("UDPDeliveredBps = %d, want shadow equivalent rate %d", got[0].UDPDeliveredBps, want)
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

	if ok := e.evaluateShadowLimited(sample, now.Add(2*time.Second)); ok {
		t.Fatal("want no shadow limited below rate gap threshold")
	}

	sample.ShadowBps = 80_000
	if ok := e.evaluateShadowLimited(sample, now.Add(4*time.Second)); ok {
		t.Fatal("first over-threshold shadow sample should only start sustain timer")
	}
	if ok := e.evaluateShadowLimited(sample, now.Add(6*time.Second)); !ok {
		t.Fatal("missing shadow limited after sustained deficit")
	}
	if status := e.snapshot(sample); !status.UDPLimited || status.TCPLimited {
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
	now := time.Unix(0, 0)
	state := qosEMAState{dataKind: transport.KindUDP, repairKind: transport.KindTCP}
	state.observe(qosSample{
		At:           now,
		Duration:     time.Second,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  4,
		DataExpected: 4,
		DataBytes:    4 * 1200,
		RepairBytes:  1200,
	})
	state.observe(qosSample{
		At:           now.Add(time.Second),
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
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, func(status qosStatus) {
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
	if len(got) != 2 {
		t.Fatalf("statuses after shadow clear = %+v, want active plus clear", got)
	}
	if got[1].UDPLimited || got[1].TCPLimited {
		t.Fatalf("clear status = %+v, want both legs clear", got[1])
	}
}

func TestQoSEstimatorDoesNotRefreshSustainedStatus(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second}, func(status qosStatus) {
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
		t.Fatalf("continued active statuses = %+v, want no duplicate", got)
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
	if len(got) != 1 {
		t.Fatalf("continued active statuses = %+v, want no refresh", got)
	}
}

func TestQoSEstimatorDoesNotRefreshActiveStatusFromOtherLegSamples(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second}, func(status qosStatus) {
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
	if len(got) != 1 {
		t.Fatalf("other leg sample statuses = %+v, want no duplicate", got)
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
	if len(got) != 1 {
		t.Fatalf("persistent active status = %+v, want no refresh", got)
	}
}

func TestQoSEstimatorFECHealthTriggersLimited(t *testing.T) {
	now := time.Unix(0, 0)
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})

	if status, ok := e.ObserveHealth(qosHealthSample{
		At:           now,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	}); ok {
		t.Fatalf("first health status = %+v, want pending only", status)
	}
	status, ok := e.ObserveHealth(qosHealthSample{
		At:           now.Add(2 * time.Second),
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	})
	if !ok {
		t.Fatal("missing health limited status after sustained unrecoverable groups")
	}
	if !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP limited", status)
	}
	if len(got) != 0 {
		t.Fatalf("callback statuses = %+v, want direct ObserveHealth caller to emit", got)
	}
}

func TestQoSEstimatorFECHealthUsesShorterSustain(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: 3 * time.Second}, nil)

	if _, ok := e.ObserveHealth(qosHealthSample{
		At:           now,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	}); ok {
		t.Fatal("first unhealthy sample should only start health sustain timer")
	}
	status, ok := e.ObserveHealth(qosHealthSample{
		At:           now.Add(qosFECHealthSustain + time.Millisecond),
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	})
	if !ok || !status.UDPLimited {
		t.Fatalf("health status = %+v ok=%t, want UDP limited after health sustain", status, ok)
	}
}

func TestQoSEstimatorFECHealthClearsAfterRecovery(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 4, Sustain: time.Second}, nil)

	if _, ok := e.ObserveHealth(qosHealthSample{
		At:           now,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	}); ok {
		t.Fatal("first unhealthy sample should only start sustain timer")
	}
	if status, ok := e.ObserveHealth(qosHealthSample{
		At:           now.Add(2 * time.Second),
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		DataArrived:  0,
		DataExpected: 4,
	}); !ok || !status.UDPLimited {
		t.Fatalf("limited status = %+v ok=%t, want UDP limited", status, ok)
	}

	var cleared qosStatus
	var clearedOK bool
	for i := 0; i < 40; i++ {
		cleared, clearedOK = e.ObserveHealth(qosHealthSample{
			At:           now.Add(3*time.Second + time.Duration(i)*100*time.Millisecond),
			DataKind:     transport.KindUDP,
			RepairKind:   transport.KindTCP,
			DataArrived:  4,
			DataExpected: 4,
		})
		if clearedOK {
			break
		}
	}
	if !clearedOK {
		t.Fatal("missing health clear after sustained healthy groups")
	}
	if cleared.UDPLimited || cleared.TCPLimited {
		t.Fatalf("clear status = %+v, want both clear", cleared)
	}
}

func TestQoSEstimatorActiveDeliveredBpsTreatsZeroAsLimited(t *testing.T) {
	e := newQoSEstimator(qosConfig{}, nil)
	e.setActive(transport.KindUDP, qosEvidenceRate, 80_000_000)
	e.setActive(transport.KindUDP, qosEvidenceHealth, 0)

	bps, ok := e.activeDeliveredBps(transport.KindUDP)
	if !ok {
		t.Fatal("missing active UDP evidence")
	}
	if bps != 0 {
		t.Fatalf("active delivered bps = %d, want health evidence zero to win", bps)
	}
}

func TestQoSEstimatorHealthyShadowClearsPriorPrimaryEvidence(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: 3 * time.Second}, nil)
	e.setActive(transport.KindUDP, qosEvidenceRate, 80_000_000)
	e.setActive(transport.KindUDP, qosEvidenceHealth, 0)
	e.limitedSince[qosEvidenceKey{kind: transport.KindUDP, evidence: qosEvidenceRate}] = now

	sample := qosEstimate{
		At:           now.Add(2 * time.Second),
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		SampleTotal:  4,
		RateGapRatio: 0.50,
		ActualBps:    50_000,
		ExpectedBps:  100_000,
		ShadowBps:    99_000,
	}
	if e.evaluateShadowLimited(sample, sample.At) {
		t.Fatal("first healthy UDP shadow sample cleared prior evidence without sustain")
	}
	if !e.kindActive(transport.KindUDP) {
		t.Fatal("UDP active evidence cleared before shadow clear sustain elapsed")
	}

	sample.At = sample.At.Add(e.cfg.Sustain + time.Millisecond)
	if !e.evaluateShadowLimited(sample, sample.At) {
		t.Fatal("healthy UDP shadow did not clear prior primary UDP evidence")
	}
	if e.kindActive(transport.KindUDP) {
		t.Fatal("UDP active evidence remained after healthy shadow clear")
	}
	if len(e.limitedSince) != 0 {
		t.Fatalf("limitedSince = %+v, want cleared pending UDP state", e.limitedSince)
	}
	status := e.snapshot(sample)
	if status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want both legs clear", status)
	}
	if status.UDPDeliveredBps != sample.ShadowBps || status.TCPDeliveredBps != sample.ActualBps {
		t.Fatalf("delivered bps = udp:%d tcp:%d, want udp:%d tcp:%d",
			status.UDPDeliveredBps, status.TCPDeliveredBps, sample.ShadowBps, sample.ActualBps)
	}
}

func TestQoSEstimatorDoesNotMarkDataLimitedWhenShadowAlreadyLimited(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second}, nil)
	e.setActive(transport.KindUDP, qosEvidenceHealth, 0)
	e.setActive(transport.KindTCP, qosEvidenceRate, 1_000)

	changed := e.evaluateLimited(qosEstimate{
		At:           now,
		DataKind:     transport.KindTCP,
		RepairKind:   transport.KindUDP,
		SampleTotal:  4,
		RateGapRatio: 0.50,
		ActualBps:    1_000,
		ShadowBps:    10_000,
	}, now)
	if !changed {
		t.Fatal("evaluateLimited did not report clearing stale data-leg evidence")
	}
	if e.isActive(transport.KindTCP, qosEvidenceRate) {
		t.Fatal("TCP rate evidence remained active while UDP shadow was already limited")
	}
	if !e.kindActive(transport.KindUDP) {
		t.Fatal("shadow limited evidence was unexpectedly cleared")
	}
}

func TestQoSEstimatorSevereRateGapUsesShorterSustain(t *testing.T) {
	now := time.Unix(0, 0)
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: 3 * time.Second}, nil)
	sample := qosEstimate{
		At:           now,
		DataKind:     transport.KindUDP,
		RepairKind:   transport.KindTCP,
		SampleTotal:  4,
		RateGapRatio: qosSevereRateGapEnter,
		ActualBps:    1_000,
		ShadowBps:    10_000,
		DeliveredBps: 1_000,
	}

	if e.evaluateLimited(sample, now) {
		t.Fatal("first severe rate sample should only start sustain timer")
	}
	sample.At = now.Add(qosSevereRateSustain + time.Millisecond)
	if !e.evaluateLimited(sample, sample.At) {
		t.Fatal("severe rate gap did not activate after shorter sustain")
	}
	if !e.isActive(transport.KindUDP, qosEvidenceRate) {
		t.Fatal("UDP rate evidence is not active after severe gap")
	}
}
