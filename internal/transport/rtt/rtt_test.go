package rtt

import "testing"

func TestEstimatorFirstSample(t *testing.T) {
	var est Estimator
	est.Add(40)

	srtt, ok := est.SRTT()
	if !ok || srtt != 40 {
		t.Fatalf("SRTT = (%d,%t), want (40,true)", srtt, ok)
	}
	rttvar, ok := est.RTTVAR()
	if !ok || rttvar != 20 {
		t.Fatalf("RTTVAR = (%d,%t), want (20,true)", rttvar, ok)
	}
	if samples := est.Samples(); samples != 1 {
		t.Fatalf("Samples = %d, want 1", samples)
	}
}

func TestEstimatorRFC6298IntegerUpdate(t *testing.T) {
	var est Estimator
	est.Add(40)
	est.Add(80)

	srtt, ok := est.SRTT()
	if !ok || srtt != 45 {
		t.Fatalf("SRTT = (%d,%t), want (45,true)", srtt, ok)
	}
	rttvar, ok := est.RTTVAR()
	if !ok || rttvar != 25 {
		t.Fatalf("RTTVAR = (%d,%t), want (25,true)", rttvar, ok)
	}

	est.Add(20)
	srtt, _ = est.SRTT()
	if srtt != 41 {
		t.Fatalf("SRTT after third sample = %d, want 41", srtt)
	}
	rttvar, _ = est.RTTVAR()
	if rttvar != 25 {
		t.Fatalf("RTTVAR after third sample = %d, want 25", rttvar)
	}
}

func TestEstimatorNoSample(t *testing.T) {
	var est Estimator
	if srtt, ok := est.SRTT(); ok || srtt != 0 {
		t.Fatalf("SRTT = (%d,%t), want (0,false)", srtt, ok)
	}
	if rttvar, ok := est.RTTVAR(); ok || rttvar != 0 {
		t.Fatalf("RTTVAR = (%d,%t), want (0,false)", rttvar, ok)
	}
}
