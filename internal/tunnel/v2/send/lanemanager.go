package send

import (
	"sync"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/ping"
)

// LegKey identifies one concrete lane transport path (spec 5.7). Active probe
// instances (ping, BwLoop) are keyed by it so the send side (which creates and
// drives them) and the recv glue (which feeds inbound PONG / BW_ACK back) find
// the same instance without either exposing transport methods to the other.
type LegKey struct {
	SessionID uint64
	LaneID    uint8
	Kind      transport.Kind
	Endpoint  string // UDP endpoint id
	Conn      string // TCP conn id
}

type LaneKey struct {
	SessionID uint64
	LaneID    uint8
}

type QoSInput interface {
	OnQoSStatus(udpLimited bool, udpDeliveredBps uint32, tcpLimited bool, tcpDeliveredBps uint32, repairCount uint8)
}

// KeyForLeg builds a LegKey from a session/lane/leg triple.
func KeyForLeg(sessionID uint64, laneID uint8, leg transport.LegRef) LegKey {
	return LegKey{
		SessionID: sessionID,
		LaneID:    laneID,
		Kind:      leg.Kind,
		Endpoint:  leg.EndpointID,
		Conn:      leg.ConnID,
	}
}

// LaneManager is the shared probe-state repository held jointly by Send and the
// recv glue (spec 5.7). It is deliberately neutral: it stores active probe
// instances plus opaque closures and understands nothing about lane/transport
// internals. This is a temporary seam — it lets the send-side active probing and
// the recv-side reply routing share state without Send exposing any transport
// method; once the boundary is clearer it can be split.
//
//   - pings:   active ping per leg. Send registers; the recv glue feeds PONG.
//   - bwLoops: the current active local BwLoop, keyed by train id. The
//     bwScheduler is session-serial, so a new loop replaces stale/aborted state;
//     the recv glue feeds BW_PROBE_ACK.
//   - remoteProbe: opaque closure the send side registers; the recv glue invokes
//     it when an inbound BW_PROBE is observed, allowing passive send state to arm
//     its scheduler only after the peer actually starts probing.
//   - remoteComplete: opaque closure the send side registers (it captures the
//     bwScheduler); the recv glue invokes it when an inbound BW_PROBE reports the
//     peer's train is done (remaining==0), driving advanceAfterRemote without the
//     recv glue knowing about the scheduler (spec 7.5).
//
// Passive paths do NOT live here: passive bw Receive and the stateless PONG
// bounce stay on the recv-glue side.
type LaneManager struct {
	mu             sync.Mutex
	pings          map[LegKey]*ping.Ping
	bwLoops        map[uint64]*bw.BwLoop
	bwActive       *bw.BwLoop
	qosInputs      map[LaneKey]QoSInput
	remoteProbe    func(key LegKey)
	remoteComplete func(key LegKey)
}

// NewLaneManager returns an empty LaneManager ready for shared use.
func NewLaneManager() *LaneManager {
	return &LaneManager{
		pings:     make(map[LegKey]*ping.Ping),
		bwLoops:   make(map[uint64]*bw.BwLoop),
		qosInputs: make(map[LaneKey]QoSInput),
	}
}

// SetRemoteProbe registers the opaque closure invoked when a peer BW_PROBE is
// observed. Send registers a closure that may lazily arm the passive-side
// bwScheduler; the recv glue never sees the scheduler.
func (m *LaneManager) SetRemoteProbe(fn func(key LegKey)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.remoteProbe = fn
	m.mu.Unlock()
}

// RemoteProbe invokes the registered remote-probe closure for key, if any.
// Called by the recv glue when an inbound BW_PROBE is observed.
func (m *LaneManager) RemoteProbe(key LegKey) {
	if m == nil {
		return
	}
	m.mu.Lock()
	fn := m.remoteProbe
	m.mu.Unlock()
	if fn != nil {
		fn(key)
	}
}

// SetRemoteComplete registers the opaque closure invoked when the peer finishes
// probing a leg (spec 7.5). Send registers a closure capturing its bwScheduler;
// the recv glue calls RemoteComplete on BW_PROBE remaining==0. Neutral seam: the
// recv glue never sees the scheduler.
func (m *LaneManager) SetRemoteComplete(fn func(key LegKey)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.remoteComplete = fn
	m.mu.Unlock()
}

// RemoteComplete invokes the registered remote-complete closure for key, if any.
// Called by the recv glue when an inbound BW_PROBE reports the peer's train done.
func (m *LaneManager) RemoteComplete(key LegKey) {
	if m == nil {
		return
	}
	m.mu.Lock()
	fn := m.remoteComplete
	m.mu.Unlock()
	if fn != nil {
		fn(key)
	}
}

// RegisterPing records the active ping for key. Send calls this when it creates
// a lane's ping (with the OnDown/OnUp closures already bound).
func (m *LaneManager) RegisterPing(key LegKey, p *ping.Ping) {
	if m == nil || p == nil {
		return
	}
	m.mu.Lock()
	m.pings[key] = p
	m.mu.Unlock()
}

func (m *LaneManager) registerPingIfAbsent(key LegKey, p *ping.Ping) bool {
	if m == nil || p == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.pings[key]; existing != nil {
		return false
	}
	m.pings[key] = p
	return true
}

// LookupPing returns the active ping for key, or nil. The recv glue calls this
// on inbound PONG to feed the matching ping.
func (m *LaneManager) LookupPing(key LegKey) *ping.Ping {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pings[key]
}

// UnregisterPing removes the ping for key (e.g. lane teardown).
func (m *LaneManager) UnregisterPing(key LegKey) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.pings, key)
	m.mu.Unlock()
}

// PutBwLoop records the current active BwLoop for trainID. Bandwidth probing is
// serialized per session, so a newly started train replaces stale/aborted loop
// state that may not have emitted a normal sample.
func (m *LaneManager) PutBwLoop(trainID uint64, l *bw.BwLoop) {
	if m == nil || l == nil {
		return
	}
	m.mu.Lock()
	m.bwLoops = make(map[uint64]*bw.BwLoop)
	m.bwLoops[trainID] = l
	m.bwActive = l
	m.mu.Unlock()
}

// LookupBwLoop returns the active BwLoop for a train/round id, or nil. ACK
// frames carry only probe_id, so when exactly one loop is active we can route to
// that loop and let BwLoop.Ack validate the round id precisely.
func (m *LaneManager) LookupBwLoop(id uint64) *bw.BwLoop {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.bwLoops[id]; l != nil {
		return l
	}
	return m.bwActive
}

// DeleteBwLoop removes the BwLoop for trainID (train complete / aborted).
func (m *LaneManager) DeleteBwLoop(trainID uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.bwLoops[trainID] == m.bwActive {
		m.bwLoops = make(map[uint64]*bw.BwLoop)
		m.bwActive = nil
	} else {
		delete(m.bwLoops, trainID)
	}
	m.mu.Unlock()
}

func (m *LaneManager) RegisterQoS(key LaneKey, input QoSInput) {
	if m == nil || input == nil {
		return
	}
	m.mu.Lock()
	m.qosInputs[key] = input
	m.mu.Unlock()
}

func (m *LaneManager) LookupQoS(key LaneKey) QoSInput {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.qosInputs[key]
}

func (m *LaneManager) DeleteQoS(key LaneKey) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.qosInputs, key)
	m.mu.Unlock()
}

// Reset drops all registered pings and BwLoops (unknown-session rebuild, spec 7:
// 重启→重连 自愈). The old session's goroutines are cancelled separately by the
// caller; this clears the stale instances so a rebuilt session starts clean. The
// remote probe/complete closures are left intact.
func (m *LaneManager) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pings = make(map[LegKey]*ping.Ping)
	m.bwLoops = make(map[uint64]*bw.BwLoop)
	m.bwActive = nil
	m.qosInputs = make(map[LaneKey]QoSInput)
	m.mu.Unlock()
}
