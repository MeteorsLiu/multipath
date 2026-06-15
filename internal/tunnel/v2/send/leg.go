package send

import (
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/observer"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
)

// role distinguishes the primary (DATA) carrier from the shadow (REPAIR)
// carrier within a lane (spec 5.2, 7.1). DATA frames take the primary role;
// REPAIR frames take the shadow role so they travel a different transport for
// path diversity when both are alive.
type role uint8

const (
	rolePrimary role = iota
	roleShadow
)

// legTransport holds one underlying transport's liveness state (spec 5.2).
// active starts false and flips true only after the peer answers
// (HELLO_ACK/PONG) — never "has a ref ⇒ alive". The TCP-only dial bookkeeping
// (dialing/backoff/retryAt) lives here too; UDP is config-injected and never
// dialed.
type legTransport struct {
	ref    transport.LegRef
	active bool // default false; HELLO_ACK/PONG → true

	// TCP-only dial state.
	dialing bool
	backoff time.Duration
	retryAt time.Time
}

// leg manages the underlying UDP+TCP transport state for one lane (spec 5.2).
// It is the internal layer beneath a lane: it owns the transport refs, the
// active flags, the selector, and the observer. The leg is never exposed
// outside send; transport.LegRef is a stateless address, the leg is the stateful
// abstraction.
//
// select rule (spec 5.2): both active → selector decides which kind is the
// DATA carrier and the other becomes the REPAIR carrier; exactly one active →
// that one carries both DATA and REPAIR (single-leg degradation, no
// notification); both dead → zero Ref (Write drops the packet).
type leg struct {
	mu          sync.Mutex
	udp         legTransport
	tcp         legTransport
	primaryKind transport.Kind    // nominal orientation: which kind anchors the primary role
	selector    selector.Selector // injected (transport/selector)
	observer    observer.Observer // trimmed quality source (delivery + RTT + PreferTCP)
}

// newLeg builds a leg with the given primary orientation and selector.
func newLeg(primaryKind transport.Kind, sel selector.Selector) leg {
	if sel == nil {
		sel = &selector.QualitySelector{}
	}
	return leg{
		primaryKind: primaryKind,
		selector:    sel,
	}
}

// bindUDP records the UDP transport ref (config-injected at lane creation). It
// does not mark the transport active; only a peer reply (HELLO_ACK/PONG) does.
func (g *leg) bindUDP(ref transport.LegRef) {
	g.mu.Lock()
	g.udp.ref = ref
	g.mu.Unlock()
}

// bindTCP records the TCP transport ref after a successful dial. It does not
// mark the transport active; the TCP HELLO handshake / first I/O does.
func (g *leg) bindTCP(ref transport.LegRef) {
	g.mu.Lock()
	g.tcp.ref = ref
	g.tcp.dialing = false
	g.mu.Unlock()
}

// markActive flips a transport to active (spec 6.2: active标准是收到HELLO_ACK或PONG).
func (g *leg) markActive(k transport.Kind) {
	g.mu.Lock()
	switch k {
	case transport.KindUDP:
		g.udp.active = true
	case transport.KindTCP:
		g.tcp.active = true
	}
	g.mu.Unlock()
}

// observeRTT feeds one RTT sample (ms) into the leg observer for the given kind
// (spec 5.4/5.5: RTT 样本回流). Called from the ping's Observer closure.
func (g *leg) observeRTT(k transport.Kind, sampleMS uint32) {
	g.observer.OnRTTSample(k, sampleMS)
}

// observeDelivery feeds one delivery outcome into the leg observer for the given
// kind (spec 5.4: onTime=true pong arrived, false ping timed out). Called from
// the ping's OnDelivery closure; drives the selector's loss-shaped QoS.
func (g *leg) observeDelivery(k transport.Kind, onTime bool) {
	g.observer.OnDelivery(k, onTime)
}

// setPreferTCP records the probeBW cold-start lock (spec 5.4/5.8). Called from
// the bwScheduler's onSample closure when a UDP sample decides TCP is preferred.
func (g *leg) setPreferTCP(prefer bool) {
	g.observer.SetPreferTCP(prefer)
}

func (g *leg) observeQoS(k transport.Kind, reason uint8, deliveredBps uint32, now time.Time) {
	g.observer.OnQoS(k, reason, deliveredBps, now)
}

// srtt returns the smoothed RTT for kind (0 if no samples). Used by the
// bwScheduler to bound the remote-wait gate fallback.
func (g *leg) srtt(k transport.Kind) time.Duration {
	switch k {
	case transport.KindUDP:
		return g.observer.UDP().SmoothedRTT
	case transport.KindTCP:
		return g.observer.TCP().SmoothedRTT
	}
	return 0
}

// markDown flips a transport to inactive (spec 6.2: UDP靠ping连续超时判死,
// TCP靠I/O error判死). The ref is retained so a recovered UDP path or a redial
// can re-activate it without re-binding.
func (g *leg) markDown(k transport.Kind) {
	g.mu.Lock()
	switch k {
	case transport.KindUDP:
		g.udp.active = false
	case transport.KindTCP:
		g.tcp.active = false
	}
	g.mu.Unlock()
}

// isActive reports whether kind is currently active.
func (g *leg) isActive(k transport.Kind) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeLocked(k)
}

func (g *leg) activeLocked(k transport.Kind) bool {
	switch k {
	case transport.KindUDP:
		return g.udp.active
	case transport.KindTCP:
		return g.tcp.active
	}
	return false
}

// anyActive reports whether at least one transport is active (lane readiness).
func (g *leg) anyActive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.udp.active || g.tcp.active
}

func (g *leg) refLocked(k transport.Kind) transport.LegRef {
	switch k {
	case transport.KindUDP:
		return g.udp.ref
	case transport.KindTCP:
		return g.tcp.ref
	}
	return transport.LegRef{}
}

// refForKind returns the bound ref for kind (zero-Kind if none), taking the lock.
// Used by the bwScheduler to enumerate probe targets.
func (g *leg) refForKind(k transport.Kind) transport.LegRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refLocked(k)
}

// selectRef returns the transport ref for the given role (spec 5.2, 7.1).
//
//   - both dead              → zero Ref (caller drops the packet)
//   - exactly one active     → that transport for both roles (single-leg
//     degradation; DATA and REPAIR share one link)
//   - both active            → selector picks the DATA carrier; the shadow role
//     gets the other kind for path diversity
//
// A zero-Kind result means "no usable transport".
func (g *leg) selectRef(r role) transport.LegRef {
	g.mu.Lock()
	defer g.mu.Unlock()

	udpActive := g.udp.active
	tcpActive := g.tcp.active

	switch {
	case !udpActive && !tcpActive:
		return transport.LegRef{}
	case udpActive && !tcpActive:
		return g.udp.ref
	case !udpActive && tcpActive:
		return g.tcp.ref
	}

	// Both active: selector chooses the DATA carrier.
	useUDP, ok := g.selector.Pick(g.qualityLocked(transport.KindUDP), g.qualityLocked(transport.KindTCP))
	if !ok {
		return transport.LegRef{}
	}

	dataKind := transport.KindTCP
	if useUDP {
		dataKind = transport.KindUDP
	}
	shadowKind := otherKind(dataKind)

	switch r {
	case roleShadow:
		return g.refLocked(shadowKind)
	default:
		return g.refLocked(dataKind)
	}
}

// qualityLocked builds the selector Quality snapshot for kind. Caller holds mu.
func (g *leg) qualityLocked(k transport.Kind) selector.Quality {
	var oq observer.Quality
	switch k {
	case transport.KindUDP:
		oq = g.observer.UDP()
	case transport.KindTCP:
		oq = g.observer.TCP()
	}
	return selector.Quality{
		Active:       g.activeLocked(k),
		DeliveryRate: oq.DeliveryRate,
		SmoothedRTT:  oq.SmoothedRTT,
		RTTVariance:  oq.RTTVariance,
		PreferTCP:    oq.PreferTCP,

		QoSActive:       oq.QoSActive,
		QoSReason:       oq.QoSReason,
		QoSDeliveredBps: oq.QoSDeliveredBps,
	}
}

func (g *leg) qualitySnapshot() (selector.Quality, selector.Quality) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.qualityLocked(transport.KindUDP), g.qualityLocked(transport.KindTCP)
}

// setPrimary flips the nominal primary orientation. The active DATA/REPAIR role
// is selected by the selector from observer quality, including received QoS
// status.
func (g *leg) setPrimary(k transport.Kind) {
	if k != transport.KindUDP && k != transport.KindTCP {
		return
	}
	g.mu.Lock()
	g.primaryKind = k
	g.mu.Unlock()
}

func otherKind(k transport.Kind) transport.Kind {
	if k == transport.KindUDP {
		return transport.KindTCP
	}
	return transport.KindUDP
}
