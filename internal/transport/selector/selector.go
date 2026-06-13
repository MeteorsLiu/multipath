// Package selector provides transport selection logic for choosing between
// UDP and TCP based on observed quality metrics (spec 5.3: leg selector).
package selector

import (
	"sync"
	"time"
)

// Quality holds the observed metrics for one transport path. Fields are limited
// to those used for QoS-driven selection (spec 5.3): liveness, delivery rate,
// RTT, and the probeBW cold-start PreferTCP lock.
type Quality struct {
	Active       bool
	DeliveryRate float64
	SmoothedRTT  time.Duration
	RTTVariance  time.Duration
	PreferTCP    bool // probeBW cold-start lock (old BandwidthPreferTCP)

	QoSActive       bool
	QoSReason       uint8
	QoSDeliveredBps uint32
}

// Selector picks between UDP and TCP based on Quality.
type Selector interface {
	Pick(udp, tcp Quality) (useUDP bool, ok bool)
}

const (
	minUDPDelivery = 0.80
	minTCPDelivery = 0.90

	QoSReasonLimited    uint8 = 1
	QoSReasonBacklogged uint8 = 2

	qosHold       = 10 * time.Second
	qosPreferWait = 10 * time.Second
	qosPreferMax  = 5 * time.Minute
	qosStable     = 60 * time.Second
)

// QualitySelector implements transport selection based on receive-side QoS
// status, delivery rate, RTT variance, and the probeBW PreferTCP lock (spec
// 5.3). Priority:
//
//  1. both dead            → ok=false
//  2. only one active      → that one
//  3. active QoS status    → avoid bad leg, or choose higher delivered bps
//  4. UDP loss high & TCP good → TCP   (ping fallback)
//  5. UDP jitter high & TCP good → TCP (ping fallback)
//  6. PreferTCP            → TCP        (probeBW cold-start lock)
//  7. default             → UDP
//
// Receive-side QoS wins over local ping fallback and PreferTCP. Ping delivery
// and RTT remain the near-idle fallback when FEC differential samples are
// silent.
type QualitySelector struct {
	mu  sync.Mutex
	now func() time.Time

	hasCurrent     bool
	currentUDP     bool
	holdUntil      time.Time
	preferWait     time.Duration
	udpSilentSince time.Time
	stableSince    time.Time
}

func NewQualitySelectorForTest(now func() time.Time) *QualitySelector {
	return &QualitySelector{now: now}
}

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
	now := s.clock()
	if useUDP, ok := s.pickQoS(udp, tcp, now); ok {
		return useUDP, true
	}

	// 3. UDP delivery poor & TCP healthy → TCP.
	if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
		s.recordPick(false, now)
		return false, true
	}
	// 4. UDP RTT variance too high & TCP healthy → TCP.
	if udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
		tcp.DeliveryRate >= minTCPDelivery {
		s.recordPick(false, now)
		return false, true
	}
	// 5. probeBW cold-start lock → TCP.
	if udp.PreferTCP {
		s.recordPick(false, now)
		return false, true
	}
	// 6. default → UDP.
	s.recordPick(true, now)
	return true, true
}

func (s *QualitySelector) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *QualitySelector) pickQoS(udp, tcp Quality, now time.Time) (bool, bool) {
	if s == nil {
		return qosPreferredStateless(udp, tcp)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.preferWait == 0 {
		s.preferWait = qosPreferWait
	}
	if s.hasCurrent && now.Before(s.holdUntil) && !(s.currentUDP && udp.QoSActive) {
		return s.currentUDP, true
	}

	preferredUDP, hasQoS := qosPreferredStateless(udp, tcp)
	if hasQoS {
		if s.hasCurrent && s.currentUDP && !preferredUDP && !s.stableSince.IsZero() && now.Sub(s.stableSince) < qosStable {
			s.preferWait *= 2
			if s.preferWait > qosPreferMax {
				s.preferWait = qosPreferMax
			}
		}
		s.setCurrentLocked(preferredUDP, now, true)
		return preferredUDP, true
	}

	if s.hasCurrent && !s.currentUDP {
		if s.udpSilentSince.IsZero() {
			s.udpSilentSince = now
			return false, true
		}
		if now.Sub(s.udpSilentSince) < s.preferWait {
			return false, true
		}
		s.setCurrentLocked(true, now, true)
		return true, true
	}

	if s.hasCurrent && s.currentUDP {
		if s.stableSince.IsZero() {
			s.stableSince = now
		}
		if now.Sub(s.stableSince) >= qosStable {
			s.preferWait = qosPreferWait
		}
	}
	return false, false
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

func (s *QualitySelector) recordPick(useUDP bool, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preferWait == 0 {
		s.preferWait = qosPreferWait
	}
	if !s.hasCurrent || s.currentUDP != useUDP {
		s.setCurrentLocked(useUDP, now, false)
		return
	}
	if useUDP && s.stableSince.IsZero() {
		s.stableSince = now
	}
}

func (s *QualitySelector) setCurrentLocked(useUDP bool, now time.Time, hold bool) {
	changed := !s.hasCurrent || s.currentUDP != useUDP
	s.hasCurrent = true
	s.currentUDP = useUDP
	if changed && hold {
		s.holdUntil = now.Add(qosHold)
	}
	if useUDP {
		s.stableSince = now
		s.udpSilentSince = time.Time{}
	} else {
		s.stableSince = time.Time{}
		if s.udpSilentSince.IsZero() {
			s.udpSilentSince = now
		}
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
