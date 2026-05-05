package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	minBandwidthProbeSamples    = 1
	bandwidthProbeLossThreshold = 0.01
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

func (q *laneQualityState) legQualities(input laneQualityInput) (LegQuality, LegQuality) {
	udpBW, tcpBW := q.bandwidth.legQualities()
	return LegQuality{
			Active:              input.udpActive,
			DeliveryRate:        q.udpDelivery.deliveryRate(),
			SmoothedRTT:         input.udpSRTT,
			RTTVariance:         input.udpRTTVar,
			BandwidthBps:        udpBW.BandwidthBps,
			ProbeLoss:           udpBW.ProbeLoss,
			ProbeSamples:        udpBW.ProbeSamples,
			BandwidthQoSLimited: udpBW.BandwidthQoSLimited,
			BandwidthPreferTCP:  udpBW.BandwidthPreferTCP,
		}, LegQuality{
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

func (q *bandwidthQualityState) legQualities() (LegQuality, LegQuality) {
	return LegQuality{
			BandwidthBps:        q.udpBandwidthBps,
			ProbeLoss:           q.udpProbeLoss,
			ProbeSamples:        q.udpProbeSamples,
			BandwidthQoSLimited: q.qosLimited,
			BandwidthPreferTCP:  q.preferTCP,
		}, LegQuality{
			BandwidthBps: q.tcpBandwidthBps,
			ProbeLoss:    q.tcpProbeLoss,
			ProbeSamples: q.tcpProbeSamples,
		}
}

func (q *bandwidthQualityState) recordSample(kind transport.Kind, bandwidthBps uint64, loss float64) {
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
	q.updateQoSState()
}

func (q *bandwidthQualityState) updateQoSState() {
	if q.udpProbeSamples < minBandwidthProbeSamples || q.tcpProbeSamples < minBandwidthProbeSamples ||
		q.udpBandwidthBps == 0 || q.tcpBandwidthBps == 0 {
		return
	}
	q.qosLimited = q.udpProbeLoss >= bandwidthProbeLossThreshold
	q.preferTCP = q.qosLimited && q.tcpBandwidthBps > q.udpBandwidthBps
}

func bandwidthLossEWMA(old, sample float64, samples uint32) float64 {
	if samples == 0 {
		return sample
	}
	return (old*7 + sample) / 8
}
