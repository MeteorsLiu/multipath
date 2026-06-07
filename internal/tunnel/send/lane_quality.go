package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send/leg"
)

const (
	minBandwidthProbeSamples    = 1
	bandwidthProbeLossThreshold = 0.01
	tcpBandwidthPreferRatio     = 3.0 / 2.0
)

type laneQualityState struct {
	udpDelivery legQualityTracker
	tcpDelivery legQualityTracker
	bandwidth   bandwidthQualityState
}

type laneQualityInput struct {
	udpActive bool
	udpSRTT   time.Duration
	udpRTTVar time.Duration
	tcpActive bool
	tcpSRTT   time.Duration
	tcpRTTVar time.Duration
}

func (q *laneQualityState) legQualities(input laneQualityInput) (leg.Quality, leg.Quality) {
	udpBW, tcpBW := q.bandwidth.legQualities()
	return leg.Quality{
			Active:              input.udpActive,
			DeliveryRate:        q.udpDelivery.deliveryRate(),
			SmoothedRTT:         input.udpSRTT,
			RTTVariance:         input.udpRTTVar,
			BandwidthBps:        udpBW.BandwidthBps,
			ProbeLoss:           udpBW.ProbeLoss,
			ProbeSamples:        udpBW.ProbeSamples,
			BandwidthQoSLimited: udpBW.BandwidthQoSLimited,
			BandwidthPreferTCP:  udpBW.BandwidthPreferTCP,
		}, leg.Quality{
			Active:       input.tcpActive,
			DeliveryRate: q.tcpDelivery.deliveryRate(),
			SmoothedRTT:  input.tcpSRTT,
			RTTVariance:  input.tcpRTTVar,
			BandwidthBps: tcpBW.BandwidthBps,
			ProbeLoss:    tcpBW.ProbeLoss,
			ProbeSamples: tcpBW.ProbeSamples,
		}
}

func (q *laneQualityState) recordDelivery(kind transport.Kind, onTime bool) {
	switch kind {
	case transport.KindUDP:
		q.udpDelivery.recordDelivery(onTime)
	case transport.KindTCP:
		q.tcpDelivery.recordDelivery(onTime)
	}
}

func (q *laneQualityState) recordBandwidthSample(kind transport.Kind, bandwidthBps uint64, loss float64) {
	q.bandwidth.recordSample(kind, bandwidthBps, loss)
}

func (q *laneQualityState) recordBandwidthSampleWithReference(kind transport.Kind, bandwidthBps uint64, loss float64, referenceBps uint64) {
	q.bandwidth.recordSampleWithReference(kind, bandwidthBps, loss, referenceBps)
}

type bandwidthQualityState struct {
	udpBandwidthBps uint64
	udpProbeLoss    float64
	udpProbeSamples uint32
	tcpBandwidthBps uint64
	tcpProbeLoss    float64
	tcpProbeSamples uint32
	qosLimited      bool
	preferTCP       bool
}

func (q *bandwidthQualityState) legQualities() (leg.Quality, leg.Quality) {
	return leg.Quality{
			BandwidthBps:        q.udpBandwidthBps,
			ProbeLoss:           q.udpProbeLoss,
			ProbeSamples:        q.udpProbeSamples,
			BandwidthQoSLimited: q.qosLimited,
			BandwidthPreferTCP:  q.preferTCP,
		}, leg.Quality{
			BandwidthBps: q.tcpBandwidthBps,
			ProbeLoss:    q.tcpProbeLoss,
			ProbeSamples: q.tcpProbeSamples,
		}
}

func (q *bandwidthQualityState) recordSample(kind transport.Kind, bandwidthBps uint64, loss float64) {
	q.recordSampleWithReference(kind, bandwidthBps, loss, 0)
}

func (q *bandwidthQualityState) recordSampleWithReference(kind transport.Kind, bandwidthBps uint64, loss float64, referenceBps uint64) {
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
	q.updateQoSState(referenceBps)
}

func (q *bandwidthQualityState) updateQoSState(referenceBps uint64) {
	if q.udpProbeSamples < minBandwidthProbeSamples || q.udpBandwidthBps == 0 {
		return
	}
	if referenceBps == 0 {
		if q.tcpProbeSamples < minBandwidthProbeSamples || q.tcpBandwidthBps == 0 {
			return
		}
		referenceBps = q.tcpBandwidthBps
	}
	q.qosLimited = q.udpProbeLoss >= bandwidthProbeLossThreshold
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
