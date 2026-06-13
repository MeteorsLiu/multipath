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
//   - bwLoops: active BwLoop per probe train id. The bwScheduler stores
//     (stage④); the recv glue feeds BW_PROBE_ACK.
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
	remoteComplete func(key LegKey)
}

// NewLaneManager returns an empty LaneManager ready for shared use.
func NewLaneManager() *LaneManager {
	return &LaneManager{
		pings:   make(map[LegKey]*ping.Ping),
		bwLoops: make(map[uint64]*bw.BwLoop),
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

// PutBwLoop records the active BwLoop for trainID. The bwScheduler calls this
// when it starts a local bandwidth train (stage④).
func (m *LaneManager) PutBwLoop(trainID uint64, l *bw.BwLoop) {
	if m == nil || l == nil {
		return
	}
	m.mu.Lock()
	m.bwLoops[trainID] = l
	m.mu.Unlock()
}

// LookupBwLoop returns the active BwLoop for trainID, or nil. The recv glue
// calls this on inbound BW_PROBE_ACK to feed the matching loop.
func (m *LaneManager) LookupBwLoop(trainID uint64) *bw.BwLoop {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bwLoops[trainID]
}

// DeleteBwLoop removes the BwLoop for trainID (train complete / aborted).
func (m *LaneManager) DeleteBwLoop(trainID uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.bwLoops, trainID)
	m.mu.Unlock()
}

// Reset drops all registered pings and BwLoops (unknown-session rebuild, spec 7:
// 重启→重连 自愈). The old session's goroutines are cancelled separately by the
// caller; this clears the stale instances so a rebuilt session starts clean. The
// remoteComplete closure is left intact — the caller re-registers it.
func (m *LaneManager) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pings = make(map[LegKey]*ping.Ping)
	m.bwLoops = make(map[uint64]*bw.BwLoop)
	m.mu.Unlock()
}
