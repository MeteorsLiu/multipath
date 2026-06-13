// Package selector provides transport selection logic for choosing between
// UDP and TCP based on observed quality metrics (spec 5.3: leg selector).
package selector

import "time"

// Quality holds the observed metrics for one transport path. Fields are limited
// to those used for QoS-driven selection (spec 5.3): liveness, delivery rate,
// RTT, and the probeBW cold-start PreferTCP lock.
type Quality struct {
	Active       bool
	DeliveryRate float64
	SmoothedRTT  time.Duration
	RTTVariance  time.Duration
	PreferTCP    bool // probeBW cold-start lock (old BandwidthPreferTCP)
}

// Selector picks between UDP and TCP based on Quality.
type Selector interface {
	Pick(udp, tcp Quality) (useUDP bool, ok bool)
}

const (
	minUDPDelivery = 0.80
	minTCPDelivery = 0.90
)

// QualitySelector implements transport selection based on delivery rate, RTT
// variance, and the probeBW PreferTCP lock (spec 5.3). Priority:
//
//	1. both dead            → ok=false
//	2. only one active      → that one
//	3. UDP loss high & TCP good → TCP   (loss-shaped QoS)
//	4. UDP jitter high & TCP good → TCP (jitter QoS)
//	5. PreferTCP            → TCP        (probeBW cold-start lock)
//	6. default             → UDP
//
// Quality signals (3-4) come before PreferTCP (5): observed quality wins, and
// PreferTCP is the cold-start fallback only when quality shows no anomaly.
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

	// Both active.
	// 3. UDP delivery poor & TCP healthy → TCP.
	if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}
	// 4. UDP RTT variance too high & TCP healthy → TCP.
	if udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
		tcp.DeliveryRate >= minTCPDelivery {
		return false, true
	}
	// 5. probeBW cold-start lock → TCP.
	if udp.PreferTCP {
		return false, true
	}
	// 6. default → UDP.
	return true, true
}

// UDPPreferSelector always prefers UDP if active, falling back to TCP (spec 5.3).
type UDPPreferSelector struct{}

func (UDPPreferSelector) Pick(udp, tcp Quality) (useUDP bool, ok bool) {
	switch {
	case udp.Active:
		return true, true
	case tcp.Active:
		return false, true
	default:
		return false, false
	}
}
