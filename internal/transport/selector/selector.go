// Package selector provides transport selection logic for choosing between
// UDP and TCP based on observed quality metrics (spec 5.3: leg selector).
package selector

// Quality holds the observed metrics for one transport path. Fields are limited
// to those used for QoS-driven selection (spec 5.3): liveness, receive-side QoS,
// and the probeBW cold-start PreferTCP lock.
type Quality struct {
	Active    bool
	PreferTCP bool // probeBW cold-start lock (old BandwidthPreferTCP)

	QoSActive       bool
	QoSDeliveredBps uint32
}

// Selector picks between UDP and TCP based on Quality.
type Selector interface {
	Pick(udp, tcp Quality) (useUDP bool, ok bool)
}

// QualitySelector implements transport selection based on receive-side QoS
// status and the probeBW PreferTCP lock (spec 5.3). Priority:
//
//  1. both dead            → ok=false
//  2. only one active      → that one
//  3. active QoS status    → avoid bad leg, or choose higher delivered bps
//  4. PreferTCP            → TCP        (probeBW cold-start lock)
//  5. default             → UDP
//
// Receive-side QoS wins over PreferTCP. Ping liveness only controls Active;
// rate/RTT-derived selector fallbacks are intentionally not used.
type QualitySelector struct{}

func (s *QualitySelector) Pick(udp, tcp Quality) (useUDP bool, ok bool) {
	switch {
	case !udp.Active && !tcp.Active:
		return false, false
	case udp.Active && !tcp.Active:
		return true, true
	case !udp.Active && tcp.Active:
		return false, true
	}

	// Both active.
	if useUDP, ok := qosPreferredStateless(udp, tcp); ok {
		return useUDP, true
	}

	// 4. probeBW cold-start lock → TCP.
	if udp.PreferTCP {
		return false, true
	}
	// 5. default → UDP.
	return true, true
}

func qosPreferredStateless(udp, tcp Quality) (bool, bool) {
	switch {
	case udp.QoSActive && !tcp.QoSActive:
		return false, true
	case !udp.QoSActive && tcp.QoSActive:
		return true, true
	case udp.QoSActive && tcp.QoSActive:
		if tcp.QoSDeliveredBps > udp.QoSDeliveredBps {
			return false, true
		}
		return true, true
	default:
		return false, false
	}
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
