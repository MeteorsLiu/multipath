package send

import (
	"time"
)

type LegQuality struct {
	Active       bool
	DeliveryRate float64
	SmoothedRTT  time.Duration
	RTTVariance  time.Duration
}

type LegSelector interface {
	Pick(udp, tcp LegQuality) (useUDP bool, ok bool)
}

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

	const minUDPDelivery = 0.80
	const minTCPDelivery = 0.90

	// Delivery rate below threshold: token-bucket policer (drops excess).
	if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}

	// RTT variance exceeds mean: shaper (bufferbloat, jitter).
	if udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
		tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}

	return true, true
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


