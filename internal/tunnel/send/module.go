package send

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/schedule"
	"github.com/MeteorsLiu/multipath/internal/schedule/cfs"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

var (
	errNoRunnableLane = errors.New("tunnel: no runnable lane")
	errUnknownLane    = errors.New("tunnel: unknown lane")
	errInvalidLane    = errors.New("tunnel: invalid lane")
)

// Send owns the send-side runtime state for the multipath tunnel.
//
// Locking convention:
//
//   - The five map mutexes (lanesMu, sessionStatesMu, helloRoutesMu, probeMu,
//     runnableCachesMu) protect the map containers and a hold is released
//     before any further work. They are NEVER held concurrently with each
//     other except in runnableLanes recompute, which acquires
//     runnableCachesMu -> lanesMu in a fixed direction.
//   - laneRuntime.mu, sendState.mu, sessionpkg.Session.mu, sessionpkg.Hello.mu
//     and cfs.Strategy.mu are owned by their respective structs and are taken
//     after releasing all map mutexes.
//   - Channel sends on packets / probeEvents are NEVER performed while holding
//     any of the above locks.
//   - Atomic fields (activeSessionID, hasActiveSession, bootstrapped,
//     fecProfile, negotiatedCaps, nextProbeTarget) need no lock.
type Send struct {
	// Immutable after construction.
	sessionManager  *sessionpkg.Manager
	streamTransport transport.StreamTransport
	probeInterval   time.Duration
	probeTimeout    time.Duration
	probeEvents     chan probe.Event
	packets         chan transport.Payload
	bootstrapLanes  []BootstrapLane

	// Mutable but lock-free.
	fecCodec fecCodec

	// Atomic flags and counters.
	activeSessionID  atomic.Uint64
	hasActiveSession atomic.Bool
	bootstrapped     atomic.Bool
	fecProfile       atomic.Uint32 // stores uint8 fec profile
	negotiatedCaps   atomic.Uint32 // stores uint16 caps
	nextProbeTarget  atomic.Uint64 // stores probe.Target

	// activeSendState caches the *sendState for the currently active session
	// so the TUN data path can skip the Manager and sessionStatesMu lookups.
	// It is populated by activateSession after the sendState has been
	// installed and cleared on session close.
	activeSendState atomic.Pointer[sendState]

	// Per-collection mutexes. See locking convention above.
	lanesMu sync.RWMutex
	lanes   map[laneKey]*laneRuntime

	sessionStatesMu sync.RWMutex
	sendStates      map[*sessionpkg.Session]*sendState
	strategies      map[uint64]schedule.Strategy[*laneRuntime]

	helloRoutesMu sync.Mutex
	helloRoutes   map[laneKey]helloRoute

	probeMu      sync.Mutex
	probeTargets map[probe.Target]probeBinding
	probeKeys    map[pingKey]probe.Target

	runnableCachesMu sync.Mutex
	runnableCaches   map[uint64]*runnableLaneCache
}

func New(configs ...Config) *Send {
	in := &Send{
		lanes:          make(map[laneKey]*laneRuntime),
		sendStates:     make(map[*sessionpkg.Session]*sendState),
		runnableCaches: make(map[uint64]*runnableLaneCache),
		helloRoutes:    make(map[laneKey]helloRoute),
		strategies:     make(map[uint64]schedule.Strategy[*laneRuntime]),
		probeTargets:   make(map[probe.Target]probeBinding),
		probeKeys:      make(map[pingKey]probe.Target),
		packets:        make(chan transport.Payload, defaultPacketQueueSize),
		sessionManager: &sessionpkg.Manager{},
	}
	for _, cfg := range configs {
		in.applyConfig(cfg)
	}
	return in
}

func (l *Send) applyConfig(cfg Config) {
	if cfg.SessionManager != nil {
		l.sessionManager = cfg.SessionManager
	}
	if cfg.StreamTransport != nil {
		l.streamTransport = cfg.StreamTransport
	}
	if cfg.ProbeInterval > 0 {
		l.probeInterval = cfg.ProbeInterval
	}
	if cfg.ProbeTimeout > 0 {
		l.probeTimeout = cfg.ProbeTimeout
	}
	if cfg.ProbeEvents != nil {
		l.probeEvents = cfg.ProbeEvents
	}
	if cfg.EnableFEC {
		l.enableFEC()
	}
	l.bootstrapLanes = append(l.bootstrapLanes, cfg.BootstrapLanes...)
}

func (l *Send) Packets() <-chan transport.Payload {
	return l.packets
}

func (l *Send) bootstrap(ctx context.Context) error {
	if !l.bootstrapped.CompareAndSwap(false, true) {
		debuglog.Printf("send", "bootstrap_skip already_bootstrapped")
		return nil
	}
	debuglog.Printf("send", "bootstrap lanes=%d", len(l.bootstrapLanes))

	for _, lane := range l.bootstrapLanes {
		caps := protocol.CapTCPFallback
		fecProfile := uint8(l.fecProfile.Load())
		if fecProfile == protocol.FECProfileSLC4Plus1 {
			caps |= protocol.CapFEC
		}
		debuglog.Printf("send", "bootstrap_lane session=%d lane=%d weight=%d leg={%s} tcp_remote=%s caps=%#x fec_profile=%d", lane.SessionID, lane.LaneID, lane.Weight, debugLeg(lane.Leg), lane.TCPRemote, caps, fecProfile)
		if err := l.startLane(ctx, startLaneConfig{
			SessionID:  lane.SessionID,
			LaneID:     lane.LaneID,
			Weight:     lane.Weight,
			Leg:        lane.Leg,
			TCPRemote:  lane.TCPRemote,
			Caps:       caps,
			FECProfile: fecProfile,
		}); err != nil {
			debuglog.Printf("send", "bootstrap_lane_err session=%d lane=%d err=%v", lane.SessionID, lane.LaneID, err)
			return err
		}
	}
	return nil
}

// activateSession records that this Send is bound to sessionID. The
// associated *sendState (if already installed) is cached so the TUN data path
// can skip Manager and sessionStatesMu lookups. Idempotent.
func (l *Send) activateSession(sessionID uint64) {
	l.activeSessionID.Store(sessionID)
	l.hasActiveSession.Store(true)

	session, ok := l.sessionManager.Get(sessionID)
	if !ok {
		return
	}
	l.sessionStatesMu.RLock()
	state := l.sendStates[session]
	l.sessionStatesMu.RUnlock()
	if state != nil {
		l.activeSendState.Store(state)
	}
}

// activeSession returns the active session id and whether it is set.
func (l *Send) activeSession() (uint64, bool) {
	return l.activeSessionID.Load(), l.hasActiveSession.Load()
}

func (l *Send) encodePacket(frame protocol.Frame) (*packetbuf.Packet, error) {
	size, err := frameEncodeCapacity(frame)
	if err != nil {
		return nil, err
	}
	return l.encodePacketWithSize(frame, size)
}

// encodePacketWithSize encodes the frame using a previously computed size hint.
// Callers that have already paid the frameEncodeCapacity switch (for example
// writeScheduledFrame to size schedule cost) should prefer this variant.
func (l *Send) encodePacketWithSize(frame protocol.Frame, size int) (*packetbuf.Packet, error) {
	packet := packetbuf.Acquire(size)
	encoded, err := protocol.Encode(frame, packet.Payload[:0])
	if err != nil {
		packet.Release()
		return nil, err
	}
	packet.Payload = encoded
	return packet, nil
}

// strategy returns the schedule strategy for sessionID, creating one on first
// use under sessionStatesMu.
func (l *Send) strategy(sessionID uint64) schedule.Strategy[*laneRuntime] {
	l.sessionStatesMu.RLock()
	strategy := l.strategies[sessionID]
	l.sessionStatesMu.RUnlock()
	if strategy != nil {
		return strategy
	}

	l.sessionStatesMu.Lock()
	defer l.sessionStatesMu.Unlock()
	if strategy := l.strategies[sessionID]; strategy != nil {
		return strategy
	}
	strategy = cfs.New[*laneRuntime]()
	l.strategies[sessionID] = strategy
	return strategy
}

func (l *Send) pickLane(sessionID uint64, cost uint32) (*laneRuntime, bool) {
	lanes := l.runnableLanes(sessionID)
	if len(lanes) == 0 {
		return nil, false
	}
	return l.strategy(sessionID).Pick(lanes, cost)
}

// runnableLanes returns the currently runnable lanes for sessionID. The cache
// is used when valid; otherwise the lanes map is snapshotted into the cache's
// backing array and each lane's readiness is checked outside lanesMu (so
// lane.mu is never nested under lanesMu).
//
// To stay consistent under concurrent dirty marks, runnableLanes takes
// ownership of the cache buffer for the duration of the recompute and only
// reinstalls it if the generation has not advanced; otherwise the recomputed
// slice is returned to the caller but the cache is left in dirty state for
// the next call to recompute.
func (l *Send) runnableLanes(sessionID uint64) []*laneRuntime {
	l.runnableCachesMu.Lock()
	cache := l.runnableCaches[sessionID]
	if cache == nil {
		cache = &runnableLaneCache{}
		l.runnableCaches[sessionID] = cache
	}
	if cache.valid {
		snapshot := cache.lanes
		l.runnableCachesMu.Unlock()
		return snapshot
	}
	// Take ownership of the cache buffer so we can fill it without holding
	// runnableCachesMu through the lanesMu/lane.mu acquisitions below.
	scratch := cache.lanes[:0]
	cache.lanes = nil
	gen := cache.generation
	l.runnableCachesMu.Unlock()

	// Snapshot lane pointers for sessionID into the scratch buffer.
	l.lanesMu.RLock()
	for key, lane := range l.lanes {
		if key.sessionID == sessionID {
			scratch = append(scratch, lane)
		}
	}
	l.lanesMu.RUnlock()

	// Filter by readiness in-place (each lane.mu is acquired briefly, no map
	// lock held).
	n := 0
	for _, lane := range scratch {
		if lane.ready() {
			scratch[n] = lane
			n++
		}
	}
	// Zero out the discarded tail so we don't keep stale pointers alive.
	for i := n; i < len(scratch); i++ {
		scratch[i] = nil
	}
	runnable := scratch[:n]
	slices.SortFunc(runnable, func(a, b *laneRuntime) int {
		return int(a.id) - int(b.id)
	})

	// Install only if no concurrent dirty mark advanced the generation.
	l.runnableCachesMu.Lock()
	cache = l.runnableCaches[sessionID]
	if cache != nil && cache.generation == gen {
		cache.lanes = runnable
		cache.valid = true
	}
	l.runnableCachesMu.Unlock()

	return runnable
}

func (l *Send) markRunnableLanesDirty(sessionID uint64) {
	l.runnableCachesMu.Lock()
	defer l.runnableCachesMu.Unlock()
	cache := l.runnableCaches[sessionID]
	if cache == nil {
		cache = &runnableLaneCache{}
		l.runnableCaches[sessionID] = cache
	}
	cache.valid = false
	cache.generation++
}

func (l *Send) deleteRunnableLanesCache(sessionID uint64) {
	l.runnableCachesMu.Lock()
	delete(l.runnableCaches, sessionID)
	l.runnableCachesMu.Unlock()
}

type runnableLaneCache struct {
	lanes      []*laneRuntime
	valid      bool
	generation uint64
}

type laneKey struct {
	sessionID uint64
	laneID    uint8
}

type fallbackDialResult struct {
	key laneKey
	leg transport.LegRef
	err error
}

type probeBinding struct {
	sessionID uint64
	laneID    uint8
	leg       transport.LegRef
}

const defaultPacketQueueSize = 1024

func (l *Send) enqueuePayload(ctx context.Context, leg transport.LegRef, payload []byte) error {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	if debuglog.Enabled() {
		debuglog.Printf("send", "enqueue_payload leg={%s} bytes=%d", debugLeg(leg), len(payload))
	}
	return l.WriteTo(ctx, leg, packet)
}

func (l *Send) enqueueFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (int, error) {
	size, err := frameEncodeCapacity(frame)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "enqueue_frame_encode_err frame=%s err=%v", debugFrameSummary(frame), err)
		}
		return 0, err
	}
	return l.enqueueFrameWithSize(ctx, leg, frame, size)
}

// enqueueFrameWithSize encodes frame using a precomputed capacity hint and
// hands the resulting packet to WriteTo. Callers that already computed the
// hint (for example writeScheduledFrame) should prefer this variant to avoid
// re-running the size switch in encodePacket.
func (l *Send) enqueueFrameWithSize(ctx context.Context, leg transport.LegRef, frame protocol.Frame, size int) (int, error) {
	packet, err := l.encodePacketWithSize(frame, size)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("send", "enqueue_frame_encode_err frame=%s err=%v", debugFrameSummary(frame), err)
		}
		return 0, err
	}
	written := len(packet.Payload)
	if debuglog.Enabled() {
		debuglog.Printf("send", "enqueue_frame frame=%s leg={%s} bytes=%d", debugFrameSummary(frame), debugLeg(leg), written)
	}
	metrics.IncCounter(metrics.ProtocolFramesTotal,
		metrics.L("direction", "tx"),
		metrics.L("type", debugFrameType(frame.Type)),
		metrics.L("session", frame.SessionID),
		metrics.L("lane", frame.LaneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	return written, l.WriteTo(ctx, leg, packet)
}

func frameEncodeCapacity(frame protocol.Frame) (int, error) {
	const headerSize = 10
	switch frame.Type {
	case protocol.TypeHELLO:
		_, ok := frame.Body.(protocol.HelloBody)
		return headerSize + 11, validFrameBody(ok)
	case protocol.TypeHELLOACK:
		_, ok := frame.Body.(protocol.HelloAckBody)
		return headerSize + 12, validFrameBody(ok)
	case protocol.TypePING, protocol.TypePONG:
		_, ok := frame.Body.(protocol.PingBody)
		return headerSize + 16, validFrameBody(ok)
	case protocol.TypeDATA:
		body, ok := frame.Body.(protocol.DataBody)
		if !ok {
			return 0, protocol.ErrInvalidFrame
		}
		return headerSize + 4 + len(body.Packet), nil
	case protocol.TypeREPAIR:
		body, ok := frame.Body.(protocol.RepairBody)
		if !ok {
			return 0, protocol.ErrInvalidFrame
		}
		return headerSize + 6 + len(body.Symbol), nil
	case protocol.TypeCLOSE:
		_, ok := frame.Body.(protocol.CloseBody)
		return headerSize + 2, validFrameBody(ok)
	default:
		return 0, protocol.ErrInvalidFrame
	}
}

func validFrameBody(ok bool) error {
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return nil
}
