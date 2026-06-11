package leg

import (
	"math/bits"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send/rtt"
)

const (
	MinBandwidthProbeSamples    = 1
	BandwidthProbeLossThreshold = 0.01
	tcpBandwidthPreferRatio     = 3.0 / 2.0
	passiveBandwidthWindow      = time.Second
	deliveryWindowSize          = 32
	deliveryMinSamples          = 8
)

type Observer struct {
	mu          sync.Mutex
	udpDelivery deliveryTracker
	tcpDelivery deliveryTracker
	udpRTT      rtt.Estimator
	tcpRTT      rtt.Estimator
	inflight    inflightTracker
	bandwidth   bandwidthState
	passive     passiveState
}

type RTTSample struct {
	SampleMS uint32
	SRTTMS   uint32
	RTTVarMS uint32
	Samples  uint32
}

type Deadline struct {
	MS uint64
}

type PongResult struct {
	Sample     RTTSample
	DeadlineMS uint64
}

func (o *Observer) UDP(active bool) Quality {
	o.mu.Lock()
	defer o.mu.Unlock()
	bw := o.bandwidth.udpQuality()
	passive := o.passive.udpQuality()
	q := Quality{
		Active:              active,
		DeliveryRate:        o.udpDelivery.rate(),
		BandwidthBps:        bw.BandwidthBps,
		ProbeLoss:           bw.ProbeLoss,
		ProbeSamples:        bw.ProbeSamples,
		BandwidthQoSLimited: bw.BandwidthQoSLimited,
		BandwidthPreferTCP:  bw.BandwidthPreferTCP,
		PassiveBandwidthBps: passive.PassiveBandwidthBps,
		PassiveSamples:      passive.PassiveSamples,
		PassiveBytes:        passive.PassiveBytes,
	}
	q.SmoothedRTT = durationOrZero(o.udpRTT.SRTT())
	q.RTTVariance = durationOrZero(o.udpRTT.RTTVAR())
	return q
}

func (o *Observer) TCP(active bool) Quality {
	o.mu.Lock()
	defer o.mu.Unlock()
	bw := o.bandwidth.tcpQuality()
	passive := o.passive.tcpQuality()
	q := Quality{
		Active:              active,
		DeliveryRate:        o.tcpDelivery.rate(),
		BandwidthBps:        bw.BandwidthBps,
		ProbeLoss:           bw.ProbeLoss,
		ProbeSamples:        bw.ProbeSamples,
		PassiveBandwidthBps: passive.PassiveBandwidthBps,
		PassiveSamples:      passive.PassiveSamples,
		PassiveBytes:        passive.PassiveBytes,
	}
	q.SmoothedRTT = durationOrZero(o.tcpRTT.SRTT())
	q.RTTVariance = durationOrZero(o.tcpRTT.RTTVAR())
	return q
}

func (o *Observer) OnPingSent(kind transport.Kind, key InflightKey, sentMS uint64, timeoutMS uint64) Deadline {
	o.mu.Lock()
	defer o.mu.Unlock()
	deadline := o.deadlineLocked(kind)
	for _, expired := range o.inflight.record(key, sentMS, deadline.MS, timeoutMS) {
		o.recordDeliveryLocked(transport.Kind(expired.Kind), false)
	}
	return deadline
}

func (o *Observer) OnPong(kind transport.Kind, key InflightKey, timeMS uint64, nowMS uint64) (PongResult, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	sampleMS, deadlineMS, ok := o.inflight.accept(key, timeMS, nowMS)
	if !ok {
		return PongResult{}, false
	}
	sample, ok := o.recordPongLocked(kind, sampleMS)
	if !ok {
		return PongResult{}, false
	}
	return PongResult{Sample: sample, DeadlineMS: deadlineMS}, true
}

func (o *Observer) OnProbeDown(target uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inflight.clearTarget(target)
}

func (o *Observer) InflightLen() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inflight.len()
}

func (o *Observer) deadlineLocked(kind transport.Kind) Deadline {
	srtt, ok := o.srttLocked(kind)
	if !ok || srtt == 0 {
		return Deadline{}
	}
	deadlineMS := uint64(srtt) * 2
	if deadlineMS < 100 {
		deadlineMS = 100
	}
	return Deadline{MS: deadlineMS}
}

func (o *Observer) OnPongSample(kind transport.Kind, sampleMS uint32) (RTTSample, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.recordPongLocked(kind, sampleMS)
}

func (o *Observer) recordPongLocked(kind transport.Kind, sampleMS uint32) (RTTSample, bool) {
	est := o.rtt(kind)
	if est == nil {
		return RTTSample{}, false
	}
	est.Add(sampleMS)
	srttMS, _ := est.SRTT()
	rttvarMS, _ := est.RTTVAR()
	return RTTSample{
		SampleMS: sampleMS,
		SRTTMS:   srttMS,
		RTTVarMS: rttvarMS,
		Samples:  est.Samples(),
	}, true
}

func (o *Observer) srttLocked(kind transport.Kind) (uint32, bool) {
	est := o.rtt(kind)
	if est == nil {
		return 0, false
	}
	return est.SRTT()
}

func (o *Observer) OnDelivery(kind transport.Kind, onTime bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.recordDeliveryLocked(kind, onTime)
}

func (o *Observer) recordDeliveryLocked(kind transport.Kind, onTime bool) {
	switch kind {
	case transport.KindUDP:
		o.udpDelivery.record(onTime)
	case transport.KindTCP:
		o.tcpDelivery.record(onTime)
	}
}

func (o *Observer) OnBandwidth(kind transport.Kind, bandwidthBps uint64, loss float64, referenceBps uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bandwidth.record(kind, bandwidthBps, loss, referenceBps)
}

func (o *Observer) OnSent(kind transport.Kind, bytes uint32, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.passive.record(kind, bytes, now)
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

type bandwidthState struct {
	udpBandwidthBps uint64
	udpProbeLoss    float64
	udpProbeSamples uint32
	tcpBandwidthBps uint64
	tcpProbeLoss    float64
	tcpProbeSamples uint32
	qosLimited      bool
	preferTCP       bool
}

func (q *bandwidthState) udpQuality() Quality {
	return Quality{
		BandwidthBps:        q.udpBandwidthBps,
		ProbeLoss:           q.udpProbeLoss,
		ProbeSamples:        q.udpProbeSamples,
		BandwidthQoSLimited: q.qosLimited,
		BandwidthPreferTCP:  q.preferTCP,
	}
}

func (q *bandwidthState) tcpQuality() Quality {
	return Quality{
		BandwidthBps: q.tcpBandwidthBps,
		ProbeLoss:    q.tcpProbeLoss,
		ProbeSamples: q.tcpProbeSamples,
	}
}

func (q *bandwidthState) record(kind transport.Kind, bandwidthBps uint64, loss float64, referenceBps uint64) {
	switch kind {
	case transport.KindUDP:
		q.udpBandwidthBps = bandwidthBps
		q.udpProbeLoss = bandwidthLossEWMA(q.udpProbeLoss, loss, q.udpProbeSamples)
		q.udpProbeSamples++
	case transport.KindTCP:
		q.tcpBandwidthBps = bandwidthBps
		q.tcpProbeLoss = bandwidthLossEWMA(q.tcpProbeLoss, loss, q.tcpProbeSamples)
		q.tcpProbeSamples++
	default:
		return
	}
	q.updateQoS(referenceBps)
}

func (q *bandwidthState) updateQoS(referenceBps uint64) {
	if q.udpProbeSamples < MinBandwidthProbeSamples || q.udpBandwidthBps == 0 {
		return
	}
	if referenceBps == 0 {
		if q.tcpProbeSamples < MinBandwidthProbeSamples || q.tcpBandwidthBps == 0 {
			return
		}
		referenceBps = q.tcpBandwidthBps
	}
	q.qosLimited = q.udpProbeLoss >= BandwidthProbeLossThreshold
	if !q.qosLimited {
		q.qosLimited = float64(q.udpBandwidthBps)*tcpBandwidthPreferRatio <= float64(referenceBps)
	}
	q.preferTCP = q.qosLimited && referenceBps >= uint64(float64(q.udpBandwidthBps)*tcpBandwidthPreferRatio)
}

func bandwidthLossEWMA(old, sample float64, samples uint32) float64 {
	if samples == 0 {
		return sample
	}
	return (old*7 + sample) / 8
}

type passiveState struct {
	udp passiveTracker
	tcp passiveTracker
}

func (s *passiveState) record(kind transport.Kind, bytes uint32, now time.Time) {
	switch kind {
	case transport.KindUDP:
		s.udp.record(bytes, now)
	case transport.KindTCP:
		s.tcp.record(bytes, now)
	}
}

func (s passiveState) udpQuality() Quality {
	return Quality{
		PassiveBandwidthBps: s.udp.bandwidthBps,
		PassiveSamples:      s.udp.samples,
		PassiveBytes:        s.udp.totalBytes,
	}
}

func (s passiveState) tcpQuality() Quality {
	return Quality{
		PassiveBandwidthBps: s.tcp.bandwidthBps,
		PassiveSamples:      s.tcp.samples,
		PassiveBytes:        s.tcp.totalBytes,
	}
}

type passiveTracker struct {
	windowStart  time.Time
	windowBytes  uint64
	totalBytes   uint64
	bandwidthBps uint64
	samples      uint32
}

func (t *passiveTracker) record(bytes uint32, now time.Time) {
	if bytes == 0 || now.IsZero() {
		return
	}
	if t.windowStart.IsZero() {
		t.windowStart = now
	}
	t.windowBytes += uint64(bytes)
	t.totalBytes += uint64(bytes)
	elapsed := now.Sub(t.windowStart)
	if elapsed < passiveBandwidthWindow {
		return
	}
	sampleBps := uint64(float64(t.windowBytes*8) / elapsed.Seconds())
	if sampleBps > 0 {
		if t.bandwidthBps == 0 {
			t.bandwidthBps = sampleBps
		} else {
			t.bandwidthBps = (t.bandwidthBps*7 + sampleBps) / 8
		}
	}
	if t.samples < ^uint32(0) {
		t.samples++
	}
	t.windowStart = now
	t.windowBytes = 0
}

func durationOrZero(ms uint32, ok bool) time.Duration {
	if !ok {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
