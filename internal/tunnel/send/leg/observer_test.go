package leg

import (
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestObserverDefaultsToHealthyDelivery(t *testing.T) {
	var obs Observer

	udp := obs.UDP(true)
	if !udp.Active {
		t.Fatal("UDP quality active = false, want true")
	}
	if udp.DeliveryRate != 1.0 {
		t.Fatalf("UDP delivery = %f, want 1.0", udp.DeliveryRate)
	}
}

func TestObserverRecordsDelivery(t *testing.T) {
	var obs Observer

	for i := 0; i < deliveryMinSamples/2; i++ {
		obs.OnDelivery(transport.KindUDP, true)
		obs.OnDelivery(transport.KindUDP, false)
	}

	if got := obs.UDP(true).DeliveryRate; got != 0.5 {
		t.Fatalf("UDP delivery = %f, want 0.5", got)
	}
	if got := obs.TCP(true).DeliveryRate; got != 1.0 {
		t.Fatalf("TCP delivery = %f, want 1.0", got)
	}
}

func TestObserverDeliveryNeedsMinSamples(t *testing.T) {
	var obs Observer

	for i := 0; i < deliveryMinSamples-1; i++ {
		obs.OnDelivery(transport.KindUDP, false)
	}
	if got := obs.UDP(true).DeliveryRate; got != 1.0 {
		t.Fatalf("UDP delivery below min samples = %f, want 1.0", got)
	}

	obs.OnDelivery(transport.KindUDP, false)
	if got := obs.UDP(true).DeliveryRate; got != 0.0 {
		t.Fatalf("UDP delivery at min samples = %f, want 0.0", got)
	}
}

func TestObserverDeliveryWindowForgetsOldSamples(t *testing.T) {
	var obs Observer

	for i := 0; i < deliveryWindowSize; i++ {
		obs.OnDelivery(transport.KindUDP, false)
	}
	if got := obs.UDP(true).DeliveryRate; got != 0.0 {
		t.Fatalf("UDP delivery after bad window = %f, want 0.0", got)
	}

	for i := 0; i < deliveryWindowSize; i++ {
		obs.OnDelivery(transport.KindUDP, true)
	}
	if got := obs.UDP(true).DeliveryRate; got != 1.0 {
		t.Fatalf("UDP delivery after recovery = %f, want 1.0", got)
	}
}

func TestObserverPingTimeoutCountsAsFailedDelivery(t *testing.T) {
	var obs Observer
	const timeoutMS = 1000

	sentMS := uint64(1000)
	for i := 0; i <= deliveryMinSamples; i++ {
		key := InflightKey{Target: 1, PingID: uint64(i + 1), SessionID: 99, LaneID: 3, Leg: ProbeLegKey{Kind: uint8(transport.KindUDP), EndpointID: "udp0"}}
		// Each ping is sent one timeout after the previous one, so recording
		// it prunes the unanswered predecessor as a failed delivery.
		obs.OnPingSent(transport.KindUDP, key, sentMS, timeoutMS)
		sentMS += timeoutMS
	}

	if got := obs.UDP(true).DeliveryRate; got != 0.0 {
		t.Fatalf("UDP delivery after ping timeouts = %f, want 0.0", got)
	}
	if got := obs.TCP(true).DeliveryRate; got != 1.0 {
		t.Fatalf("TCP delivery = %f, want 1.0", got)
	}
}

func TestObserverRecordsRTTAndDeadline(t *testing.T) {
	var obs Observer
	key := InflightKey{Target: 1, PingID: 7, SessionID: 99, LaneID: 3, Leg: ProbeLegKey{Kind: uint8(transport.KindUDP), EndpointID: "udp0"}}

	if deadline := obs.OnPingSent(transport.KindUDP, key, 1000, 1000); deadline.MS != 0 {
		t.Fatalf("cold deadline = %d, want 0", deadline.MS)
	}

	result, ok := obs.OnPong(transport.KindUDP, key, 1000, 1040)
	if !ok {
		t.Fatal("OnPong returned false")
	}
	sample := result.Sample
	if sample.SampleMS != 40 || sample.SRTTMS != 40 || sample.RTTVarMS != 20 || sample.Samples != 1 {
		t.Fatalf("RTT sample = %+v, want sample=40 srtt=40 rttvar=20 samples=1", sample)
	}
	q := obs.UDP(true)
	if q.SmoothedRTT != 40*time.Millisecond || q.RTTVariance != 20*time.Millisecond {
		t.Fatalf("UDP RTT = (%s,%s), want (40ms,20ms)", q.SmoothedRTT, q.RTTVariance)
	}
	key.PingID = 8
	if deadline := obs.OnPingSent(transport.KindUDP, key, 2000, 1000); deadline.MS != 100 {
		t.Fatalf("warm deadline = %d, want 100", deadline.MS)
	}
}

func TestObserverRecordsBandwidth(t *testing.T) {
	var obs Observer

	obs.OnBandwidth(transport.KindUDP, 50_000_000, 0.10, 0)
	obs.OnBandwidth(transport.KindTCP, 100_000_000, 0, 0)

	udp := obs.UDP(true)
	tcp := obs.TCP(true)
	if udp.BandwidthBps != 50_000_000 || tcp.BandwidthBps != 100_000_000 {
		t.Fatalf("bandwidth = udp %d tcp %d, want 50M/100M", udp.BandwidthBps, tcp.BandwidthBps)
	}
	if !udp.BandwidthQoSLimited || !udp.BandwidthPreferTCP {
		t.Fatalf("UDP bandwidth flags = qos %t preferTCP %t, want both true", udp.BandwidthQoSLimited, udp.BandwidthPreferTCP)
	}
}

func TestObserverRecordsPassiveBandwidth(t *testing.T) {
	var obs Observer
	start := time.Unix(0, 0)

	obs.OnSent(transport.KindUDP, 1_000, start)
	obs.OnSent(transport.KindUDP, 1_000, start.Add(time.Second))

	udp := obs.UDP(true)
	if udp.PassiveSamples != 1 {
		t.Fatalf("passive samples = %d, want 1", udp.PassiveSamples)
	}
	if udp.PassiveBytes != 2_000 {
		t.Fatalf("passive bytes = %d, want 2000", udp.PassiveBytes)
	}
	if udp.PassiveBandwidthBps != 16_000 {
		t.Fatalf("passive bandwidth = %d, want 16000", udp.PassiveBandwidthBps)
	}
}
