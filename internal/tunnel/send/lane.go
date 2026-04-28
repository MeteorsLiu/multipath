package send

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

var errLaneUnavailable = errors.New("tunnel: lane has no usable transport leg")

// laneRuntime carries the per-lane runtime state. All mutable fields are
// guarded by mu; weight is exposed as an atomic so the schedule strategy can
// read it without grabbing mu.
type laneRuntime struct {
	id     uint8
	weight atomic.Uint32

	mu              sync.Mutex
	udpReady        bool
	udpLeg          transport.LegRef
	tcpReady        bool
	tcpLeg          transport.LegRef
	tcpRemote       string
	helloCaps       uint16
	helloFECProfile uint8
	fallbackDialing bool
}

// laneSnapshot is a value-copy of laneRuntime mutable fields, returned by
// snapshot() so callers can format diagnostics without holding mu.
type laneSnapshot struct {
	udpReady        bool
	udpLeg          transport.LegRef
	tcpReady        bool
	tcpLeg          transport.LegRef
	tcpRemote       string
	helloCaps       uint16
	helloFECProfile uint8
	fallbackDialing bool
}

func newLaneRuntime(id uint8, weight uint32) *laneRuntime {
	l := &laneRuntime{id: id}
	l.weight.Store(weight)
	return l
}

func (l *laneRuntime) Weight() uint32 {
	if l == nil {
		return 0
	}
	return l.weight.Load()
}

func (l *laneRuntime) setWeight(w uint32) {
	if l == nil {
		return
	}
	l.weight.Store(w)
}

func (l *laneRuntime) ready() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.selectLegLocked()
	return ok
}

func (l *laneRuntime) selectLeg() (transport.LegRef, bool) {
	if l == nil {
		return transport.LegRef{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.selectLegLocked()
}

func (l *laneRuntime) selectLegLocked() (transport.LegRef, bool) {
	if l.udpReady && l.udpLeg.EndpointID != "" && l.udpLeg.RemoteAddr != nil {
		return l.udpLeg, true
	}
	if l.tcpReady && l.tcpLeg.ConnID != "" {
		return l.tcpLeg, true
	}
	return transport.LegRef{}, false
}

func legCharge(leg transport.LegRef, payloadLen int) uint32 {
	if leg.Kind == transport.KindTCP {
		return uint32(payloadLen + 2)
	}
	return uint32(payloadLen)
}

// observeLeg records the latest leg observed for this lane and marks readiness
// based on the leg's identifiers.
func (l *laneRuntime) observeLeg(leg transport.LegRef) {
	l.bindLeg(leg)
}

// bindLeg is the lock-taking variant used by code paths and tests that want to
// install a leg without first acquiring lane.mu.
func (l *laneRuntime) bindLeg(leg transport.LegRef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bindLegLocked(leg)
}

// rememberLeg stores leg identifiers without flipping readiness. Used to
// remember a leg that has not yet completed its handshake.
func (l *laneRuntime) rememberLeg(leg transport.LegRef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch leg.Kind {
	case transport.KindUDP:
		l.udpLeg = leg
	case transport.KindTCP:
		l.tcpLeg = leg
	}
}

func (l *laneRuntime) bindLegLocked(leg transport.LegRef) {
	switch leg.Kind {
	case transport.KindUDP:
		l.udpLeg = leg
		l.udpReady = leg.EndpointID != "" && leg.RemoteAddr != nil
	case transport.KindTCP:
		l.tcpLeg = leg
		l.tcpReady = leg.ConnID != ""
	}
}

func (l *laneRuntime) markUDPNotReady() {
	l.mu.Lock()
	l.udpReady = false
	l.mu.Unlock()
}

func (l *laneRuntime) markTCPNotReady() {
	l.mu.Lock()
	l.tcpReady = false
	l.mu.Unlock()
}

func (l *laneRuntime) setTCPRemote(remote string) {
	l.mu.Lock()
	l.tcpRemote = remote
	l.mu.Unlock()
}

func (l *laneRuntime) tcpRemoteSnapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tcpRemote
}

func (l *laneRuntime) setHelloProfile(caps uint16, prof uint8) {
	l.mu.Lock()
	l.helloCaps = caps
	l.helloFECProfile = prof
	l.mu.Unlock()
}

func (l *laneRuntime) helloProfile() (uint16, uint8) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.helloCaps, l.helloFECProfile
}

// tryStartFallback atomically checks the lane is eligible for a TCP fallback
// dial and, if so, marks the lane as currently dialing. It returns the
// remembered tcp remote and true on success, or empty/false otherwise.
func (l *laneRuntime) tryStartFallback(streamAvailable bool) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !streamAvailable || l.fallbackDialing || l.tcpReady || l.tcpRemote == "" {
		return "", false
	}
	l.fallbackDialing = true
	return l.tcpRemote, true
}

func (l *laneRuntime) clearFallbackDialing() {
	l.mu.Lock()
	l.fallbackDialing = false
	l.mu.Unlock()
}

// legs returns the udp and tcp legs as a coherent snapshot.
func (l *laneRuntime) legs() (transport.LegRef, transport.LegRef) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.udpLeg, l.tcpLeg
}

func (l *laneRuntime) snapshot() laneSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return laneSnapshot{
		udpReady:        l.udpReady,
		udpLeg:          l.udpLeg,
		tcpReady:        l.tcpReady,
		tcpLeg:          l.tcpLeg,
		tcpRemote:       l.tcpRemote,
		helloCaps:       l.helloCaps,
		helloFECProfile: l.helloFECProfile,
		fallbackDialing: l.fallbackDialing,
	}
}
