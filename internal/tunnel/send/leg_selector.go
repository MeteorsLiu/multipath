package send

import (
	"time"
)

type LegQuality struct {
	Active              bool
	DeliveryRate        float64
	SmoothedRTT         time.Duration
	RTTVariance         time.Duration
	BandwidthBps        uint64
	ProbeLoss           float64
	ProbeSamples        uint32
	BandwidthQoSLimited bool
	BandwidthPreferTCP  bool
}

type LegSelector interface {
	Pick(udp, tcp LegQuality) (useUDP bool, ok bool)
}

const (
	minUDPDelivery = 0.80
	minTCPDelivery = 0.90
)

type QualityLegSelector struct{}

func (QualityLegSelector) Pick(udp, tcp LegQuality) (useUDP bool, ok bool) {
	switch {
	case !udp.Active && !tcp.Active:
		return false, false
	case udp.Active && !tcp.Active:
		return true, true
	case !udp.Active && tcp.Active:
		return false, true
	}

	if deliveryPrefersTCP(udp, tcp) {
		return false, true
	}

	if rttPrefersTCP(udp, tcp) {
		return false, true
	}

	if bandwidthPrefersTCP(udp, tcp) {
		return false, true
	}

	return true, true
}

func deliveryPrefersTCP(udp, tcp LegQuality) bool {
	// Delivery rate below threshold: token-bucket policer (drops excess).
	return udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery
}

func rttPrefersTCP(udp, tcp LegQuality) bool {
	// RTT variance exceeds mean: shaper (bufferbloat, jitter).
	return udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
		tcp.DeliveryRate >= minTCPDelivery
}

func bandwidthPrefersTCP(udp, tcp LegQuality) bool {
	return udp.BandwidthPreferTCP && tcp.ProbeSamples >= minBandwidthProbeSamples
}

func bandwidthOnlyPrefersTCP(udp, tcp LegQuality) bool {
	return bandwidthPrefersTCP(udp, tcp) && !deliveryPrefersTCP(udp, tcp) && !rttPrefersTCP(udp, tcp)
}

type UDPPreferssSelector struct{}

func (UDPPreferssSelector) Pick(udp, tcp LegQuality) (useUDP bool, ok bool) {
	switch {
	case udp.Active:
		return true, true
	case tcp.Active:
		return false, true
	default:
		return false, false
	}
}
