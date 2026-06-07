package leg

import "time"

type Quality struct {
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

type Selector interface {
	Pick(udp, tcp Quality) (useUDP bool, ok bool)
}

const (
	minUDPDelivery = 0.80
	minTCPDelivery = 0.90
)

type QualitySelector struct{}

func (QualitySelector) Pick(udp, tcp Quality) (useUDP bool, ok bool) {
	switch {
	case !udp.Active && !tcp.Active:
		return false, false
	case udp.Active && !tcp.Active:
		return true, true
	case !udp.Active && tcp.Active:
		return false, true
	}

	if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}

	if udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
		tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}

	if udp.BandwidthPreferTCP {
		return false, true
	}

	return true, true
}

type UDPPrefersSelector struct{}

func (UDPPrefersSelector) Pick(udp, tcp Quality) (useUDP bool, ok bool) {
	switch {
	case udp.Active:
		return true, true
	case tcp.Active:
		return false, true
	default:
		return false, false
	}
}
