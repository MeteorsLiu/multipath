// Package observer tracks transport quality metrics for selector decision-making
// (spec 5.4: leg observer). It computes delivery rate and RTT per transport kind.
package observer

import (
	"math/bits"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/rtt"
)

const (
	deliveryWindowSize = 32
	deliveryMinSamples = 8
	qosTTL             = 3 * time.Second
)

// Observer tracks delivery rate and RTT for UDP and TCP (spec 5.4). It exposes
// only the Quality fields needed by selector (DeliveryRate + RTT + PreferTCP).
type Observer struct {
	mu          sync.Mutex
	udpDelivery deliveryTracker
	tcpDelivery deliveryTracker
	udpRTT      rtt.Estimator
	tcpRTT      rtt.Estimator
	preferTCP   bool // set by bw Sample (stage④): probeBW cold-start lock
	udpQoS      qosStatus
	tcpQoS      qosStatus
}

// Quality holds the observed metrics for one transport (spec 5.4).
type Quality struct {
	DeliveryRate float64
	SmoothedRTT  time.Duration
	RTTVariance  time.Duration
	PreferTCP    bool // probeBW cold-start lock (UDP-side only)

	QoSActive       bool
	QoSReason       uint8
	QoSDeliveredBps uint32
}

type qosStatus struct {
	active       bool
	reason       uint8
	deliveredBps uint32
	updatedAt    time.Time
}

// UDP returns the current UDP quality snapshot.
func (o *Observer) UDP() Quality {
	return o.UDPAt(time.Now())
}

func (o *Observer) UDPAt(now time.Time) Quality {
	o.mu.Lock()
	defer o.mu.Unlock()
	q := Quality{
		DeliveryRate: o.udpDelivery.rate(),
		PreferTCP:    o.preferTCP,
	}
	q.SmoothedRTT = durationOrZero(o.udpRTT.SRTT())
	q.RTTVariance = durationOrZero(o.udpRTT.RTTVAR())
	q.applyQoS(o.udpQoS, now)
	return q
}

// SetPreferTCP records the probeBW cold-start decision (spec 5.4: PreferTCP). It
// is the only bandwidth-derived signal the trimmed observer keeps; bw Sample in
// stage④ calls this. Selector rule 5 reads it via UDP().PreferTCP.
func (o *Observer) SetPreferTCP(prefer bool) {
	o.mu.Lock()
	o.preferTCP = prefer
	o.mu.Unlock()
}

// TCP returns the current TCP quality snapshot.
func (o *Observer) TCP() Quality {
	return o.TCPAt(time.Now())
}

func (o *Observer) TCPAt(now time.Time) Quality {
	o.mu.Lock()
	defer o.mu.Unlock()
	q := Quality{
		DeliveryRate: o.tcpDelivery.rate(),
	}
	q.SmoothedRTT = durationOrZero(o.tcpRTT.SRTT())
	q.RTTVariance = durationOrZero(o.tcpRTT.RTTVAR())
	q.applyQoS(o.tcpQoS, now)
	return q
}

func (q *Quality) applyQoS(status qosStatus, now time.Time) {
	if !status.active || now.Sub(status.updatedAt) > qosTTL {
		return
	}
	q.QoSActive = true
	q.QoSReason = status.reason
	q.QoSDeliveredBps = status.deliveredBps
}

// OnDelivery records a delivery sample (onTime=true if within deadline, false if late/lost).
func (o *Observer) OnDelivery(kind transport.Kind, onTime bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch kind {
	case transport.KindUDP:
		o.udpDelivery.record(onTime)
	case transport.KindTCP:
		o.tcpDelivery.record(onTime)
	}
}

func (o *Observer) OnQoS(kind transport.Kind, reason uint8, deliveredBps uint32, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	status := qosStatus{
		active:       true,
		reason:       reason,
		deliveredBps: deliveredBps,
		updatedAt:    now,
	}
	switch kind {
	case transport.KindUDP:
		o.udpQoS = status
	case transport.KindTCP:
		o.tcpQoS = status
	}
}

// OnRTTSample records an RTT sample in milliseconds.
func (o *Observer) OnRTTSample(kind transport.Kind, sampleMS uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	est := o.rtt(kind)
	if est != nil {
		est.Add(sampleMS)
	}
}

func (o *Observer) rtt(kind transport.Kind) *rtt.Estimator {
	switch kind {
	case transport.KindUDP:
		return &o.udpRTT
	case transport.KindTCP:
		return &o.tcpRTT
	default:
		return nil
	}
}

// deliveryTracker keeps the last deliveryWindowSize samples in a bitmask so
// the rate reflects recent link quality instead of a lifetime average.
type deliveryTracker struct {
	window  uint64
	samples uint32
}

func (t *deliveryTracker) record(onTime bool) {
	t.window <<= 1
	if onTime {
		t.window |= 1
	}
	if t.samples < deliveryWindowSize {
		t.samples++
	}
}

func (t *deliveryTracker) rate() float64 {
	if t.samples < deliveryMinSamples {
		return 1.0
	}
	mask := ^uint64(0) >> (64 - t.samples)
	return float64(bits.OnesCount64(t.window&mask)) / float64(t.samples)
}

func durationOrZero(ms uint32, ok bool) time.Duration {
	if !ok {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
