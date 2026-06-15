package observer

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

// TestObserverDeliveryRate verifies the windowed delivery rate reflects recent
// samples and that fewer than deliveryMinSamples reports a perfect 1.0 rate so
// a fresh transport is never penalized (spec 5.4).
func TestObserverDeliveryRate(t *testing.T) {
	var o Observer

	// Below the minimum sample count: optimistic 1.0.
	o.OnDelivery(transport.KindUDP, false)
	if got := o.UDP().DeliveryRate; got != 1.0 {
		t.Fatalf("UDP delivery rate with 1 sample = %v, want 1.0", got)
	}

	// Record a clear majority of failures past the minimum.
	for i := 0; i < 10; i++ {
		o.OnDelivery(transport.KindUDP, false)
	}
	if got := o.UDP().DeliveryRate; got >= 0.5 {
		t.Fatalf("UDP delivery rate after losses = %v, want < 0.5", got)
	}

	// TCP is tracked independently.
	for i := 0; i < 10; i++ {
		o.OnDelivery(transport.KindTCP, true)
	}
	if got := o.TCP().DeliveryRate; got != 1.0 {
		t.Fatalf("TCP delivery rate all on-time = %v, want 1.0", got)
	}
}

// TestObserverRTT verifies RTT samples flow into the per-kind estimator and are
// surfaced as durations.
func TestObserverRTT(t *testing.T) {
	var o Observer

	if got := o.UDP().SmoothedRTT; got != 0 {
		t.Fatalf("UDP SRTT with no samples = %v, want 0", got)
	}

	o.OnRTTSample(transport.KindUDP, 40)
	if got := o.UDP().SmoothedRTT; got != 40*time.Millisecond {
		t.Fatalf("UDP SRTT after one 40ms sample = %v, want 40ms", got)
	}
	if got := o.UDP().RTTVariance; got != 20*time.Millisecond {
		t.Fatalf("UDP RTTVar after one 40ms sample = %v, want 20ms", got)
	}

	// TCP estimator is separate and still empty.
	if got := o.TCP().SmoothedRTT; got != 0 {
		t.Fatalf("TCP SRTT should be 0 after only UDP samples, got %v", got)
	}
}

// TestObserverPreferTCP verifies the cold-start probeBW lock is UDP-side only
// and reflected in the snapshot (spec 5.4: PreferTCP).
func TestObserverPreferTCP(t *testing.T) {
	var o Observer

	if o.UDP().PreferTCP {
		t.Fatal("PreferTCP defaults to true, want false")
	}

	o.SetPreferTCP(true)
	if !o.UDP().PreferTCP {
		t.Fatal("PreferTCP not set after SetPreferTCP(true)")
	}

	o.SetPreferTCP(false)
	if o.UDP().PreferTCP {
		t.Fatal("PreferTCP not cleared after SetPreferTCP(false)")
	}
}

func TestObserverQoSExpiresByTTL(t *testing.T) {
	var o Observer
	now := time.Unix(0, 0)
	if q := o.UDPAt(now); q.QoSSeen {
		t.Fatalf("UDP QoSSeen before status = %+v, want false", q)
	}
	o.OnQoS(transport.KindUDP, 1, 2_000_000, now)
	if q := o.UDPAt(now.Add(299 * time.Second)); !q.QoSActive || q.QoSReason != 1 || q.QoSDeliveredBps != 2_000_000 {
		t.Fatalf("UDP QoS before TTL = %+v, want active", q)
	}
	if q := o.UDPAt(now.Add(301 * time.Second)); q.QoSActive || !q.QoSSeen {
		t.Fatalf("UDP QoS after TTL = %+v, want inactive but seen", q)
	}
	if q := o.TCPAt(now.Add(301 * time.Second)); !q.QoSSeen {
		t.Fatalf("TCP QoSSeen after UDP status = %+v, want true", q)
	}
}
