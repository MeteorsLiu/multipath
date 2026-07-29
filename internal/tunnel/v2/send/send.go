package send

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/schedule"
	"github.com/MeteorsLiu/multipath/internal/schedule/drr"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/transport/selector"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/ping"
)

var (
	ErrNoRunnableLane    = errors.New("send: no runnable lane")
	ErrUnknownLane       = errors.New("send: unknown lane")
	ErrLaneUnavailable   = errors.New("send: lane unavailable")
	ErrSessionIDConflict = errors.New("send: session id conflict")
)

const (
	defaultMTUBytes               = 1500
	drrBaseQuantum                = 4 * defaultMTUBytes
	maxFECSourceSpan              = 4
	maxTxGroupID           uint32 = 1<<30 - 1
	defaultFECFlushMin            = 5 * time.Millisecond
	defaultFECFlushMax            = 30 * time.Millisecond
	debugQueueWaitLogAfter        = time.Millisecond
)

// Send owns the send-side runtime state per spec section 5.2.
//
// Public API (per spec):
//   - Write(ctx, packet) - TUN data entry point, runs scheduler
//   - WriteFrame(ctx, frame, to) - control frame entry point, no scheduler
//   - WriteTo(ctx, leg, packet) - transport output
//   - Packets() - transport output channel
type Send struct {
	// Immutable after construction
	sessionManager  *sessionpkg.Manager
	laneManager     *LaneManager
	streamTransport transport.StreamTransport
	bootstrapLanes  []BootstrapLane
	packets         chan transport.Payload
	fecConfigured   atomic.Bool
	fecFlushMin     time.Duration
	fecFlushMax     time.Duration

	// Probe tuning for active ping (spec 5.5). Used when send creates a lane's
	// ping. Zero values fall back to ping defaults.
	probeInterval time.Duration
	probeTimeout  time.Duration

	// Bandwidth probing (spec 5.8). isClient (the端 that sent HELLO) starts the
	// gate in the Local phase; bwCapBps>0 restricts probing to UDP. The
	// bwScheduler cold-starts once when enabled; uncapped client probes wait for
	// the TCP reference leg before taking the scheduler snapshot.
	isClient    bool
	enableBW    bool
	bwCapBps    uint64
	bwReference uint64
	bwSched     *bwScheduler

	// FEC codecs indexed by source span and repair count.
	fecCodecs [maxFECSourceSpan + 1][5]*fec.Codec

	// Atomic state
	activeSessionID  atomic.Uint64
	hasActiveSession atomic.Bool
	bootstrapped     atomic.Bool

	// Rebootstrap support (unknown-session self-heal, spec 7: 重启→重连 自愈).
	// baseCtx is the long-lived context passed to Bootstrap; Rebootstrap derives
	// each session's goroutines from it. sessionCancel tears down the current
	// session's goroutines (Hello loop, ping, dialer, bwScheduler) before a
	// rebuild so a forgotten session does not get resurrected by stale loops.
	rebootMu      sync.Mutex
	rebootstrapMu sync.Mutex
	baseCtx       context.Context
	sessionCtx    context.Context
	sessionCancel context.CancelFunc

	// Per-collection mutexes
	lanesMu sync.RWMutex
	lanes   map[laneKey]*laneRuntime

	strategiesMu sync.RWMutex
	strategies   map[uint64]schedule.Strategy[*laneRuntime]

	// Per-session send state
	sendStatesMu sync.RWMutex
	sendStates   map[uint64]*sendState

	runnableCacheMu sync.Mutex
	runnableCache   map[uint64]*runnableCache
}

type laneKey struct {
	sessionID uint64
	laneID    uint8
}

type runnableCache struct {
	lanes      []*laneRuntime
	valid      bool
	generation uint64
}

// sendState holds per-session send-side state.
type sendState struct {
	nextRepairKey atomic.Uint32
	fecEnabled    atomic.Bool
}

// New creates a new Send instance.
func New(configs ...Config) *Send {
	s := &Send{
		lanes:          make(map[laneKey]*laneRuntime),
		strategies:     make(map[uint64]schedule.Strategy[*laneRuntime]),
		sendStates:     make(map[uint64]*sendState),
		runnableCache:  make(map[uint64]*runnableCache),
		packets:        make(chan transport.Payload, 1024),
		sessionManager: &sessionpkg.Manager{},
		fecFlushMin:    defaultFECFlushMin,
		fecFlushMax:    defaultFECFlushMax,
	}

	for _, cfg := range configs {
		s.applyConfig(cfg)
	}

	// A LaneManager is always present: it is the shared probe-state seam between
	// send and the recv glue (spec 5.7). If the caller did not inject one, create
	// it here so send can register pings even in standalone use.
	if s.laneManager == nil {
		s.laneManager = NewLaneManager()
	}
	s.installBandwidthRemoteProbe()

	return s
}

// LaneManager returns the shared probe-state repository (spec 5.7). The recv glue
// holds the same instance (injected via Config or read from here) so inbound
// PONG / BW_ACK can reach the send-side active probe instances.
func (s *Send) LaneManager() *LaneManager {
	return s.laneManager
}

func (s *Send) applyConfig(cfg Config) {
	if cfg.SessionManager != nil {
		s.sessionManager = cfg.SessionManager
	}
	if cfg.LaneManager != nil {
		s.laneManager = cfg.LaneManager
	}
	if cfg.StreamTransport != nil {
		s.streamTransport = cfg.StreamTransport
	}
	if cfg.ProbeInterval > 0 {
		s.probeInterval = cfg.ProbeInterval
	}
	if cfg.ProbeTimeout > 0 {
		s.probeTimeout = cfg.ProbeTimeout
	}
	if cfg.IsClient {
		s.isClient = true
	}
	if cfg.EnableBandwidthProbe {
		s.enableBW = true
	}
	if cfg.BWCapBps > 0 {
		s.bwCapBps = cfg.BWCapBps
	}
	if cfg.BWReferenceBps > 0 {
		s.bwReference = cfg.BWReferenceBps
	}
	s.bootstrapLanes = append(s.bootstrapLanes, cfg.BootstrapLanes...)
}

func (s *Send) FECEnabled() bool {
	return s.fecConfigured.Load()
}

func (s *Send) EnableFEC() {
	s.fecConfigured.Store(true)
	for i := 1; i <= maxFECSourceSpan; i++ {
		for repairs := 1; repairs <= 4; repairs++ {
			s.fecCodecs[i][repairs], _ = fec.NewCodec(i, repairs)
		}
	}
	if sessionID, ok := s.activeSession(); ok {
		s.enableSessionFEC(sessionID)
	}
}

func (s *Send) enableSessionFEC(sessionID uint64) {
	if !s.FECEnabled() {
		return
	}
	state := s.getSendState(sessionID)
	if state == nil {
		return
	}
	state.fecEnabled.Store(true)
}

func (s *Send) sessionFECEnabled(sessionID uint64) bool {
	state := s.getSendState(sessionID)
	return state != nil && state.fecEnabled.Load()
}

// Packets returns the transport output channel.
func (s *Send) Packets() <-chan transport.Payload {
	return s.packets
}

// Bootstrap initializes the send instance with bootstrap lanes.
func (s *Send) Bootstrap(ctx context.Context) error {
	if !s.bootstrapped.CompareAndSwap(false, true) {
		return nil
	}

	if len(s.bootstrapLanes) == 0 {
		return nil
	}

	s.setBaseContext(ctx)

	return s.bootstrapSession(ctx)
}

// bootstrapSession stands up one active session + lanes and starts its
// self-driving goroutines (Hello retry, per-lane ping, TCP dialer, bandwidth
// scheduler), all derived from a per-session context so Rebootstrap can cancel
// them as a unit. Bootstrap and Rebootstrap both call this.
func (s *Send) bootstrapSession(ctx context.Context) error {
	// Derive a per-session context. Cancelling it tears down every goroutine this
	// session starts, so a forgotten session is not resurrected by stale loops.
	sessionCtx, cancel := context.WithCancel(ctx)
	s.setSessionContext(sessionCtx, cancel)

	// Create local session
	session, err := sessionpkg.New()
	if err != nil {
		cancel()
		return err
	}

	var sessionID uint64
	session.Do(func(v sessionpkg.View) error {
		sessionID = v.SessionID()
		return nil
	})

	if !s.sessionManager.Add(session) {
		cancel()
		return ErrSessionIDConflict
	}

	s.activateSession(sessionID)

	// Create sendState
	s.sendStatesMu.Lock()
	s.sendStates[sessionID] = &sendState{}
	s.sendStatesMu.Unlock()

	// Create bootstrap lanes
	for _, bl := range s.bootstrapLanes {
		key := laneKey{sessionID: sessionID, laneID: bl.LaneID}
		lane := newLaneRuntime(bl.LaneID, bl.Weight)

		// Bind the bootstrap UDP transport ref. Binding records the address but
		// does NOT mark the leg active — only a peer reply (HELLO_ACK/PONG) does
		// (spec 6.2). TCP is dialed lazily by the leg's dialer (stage② wiring).
		lane.bindUDP(bl.Leg)

		s.lanesMu.Lock()
		s.lanes[key] = lane
		s.lanesMu.Unlock()
		s.registerLaneQoS(sessionID, lane)

		// Open Hello with a self-driven retry loop (spec 5.1, 7.2). The sender
		// closure builds the HELLO frame and writes it on this lane's bootstrap
		// transport; nonce is delivered via the View. onExpire marks the leg down.
		laneID := bl.LaneID
		legRef := bl.Leg
		s.openLaneHello(sessionCtx, session, sessionID, lane, laneID, legRef)

		// Create and drive the active ping for this lane's UDP leg (spec 5.5, 7.3).
		// The ping starts dead unless the leg has already been activated by a
		// same-leg HELLO_ACK. Subsequent MaxLoss timeouts fire OnDown →
		// leg.markDown. The ping is registered in the shared LaneManager so the
		// recv glue can route inbound PONG to it without touching send.
		s.startLanePing(sessionCtx, sessionID, lane, legRef)

		// Start the TCP dialer for this lane if a TCP remote is configured and a
		// stream transport is available (spec 5.6). The dial runs off the data
		// path; on success it binds the TCP ref and opens a HELLO handshake on that
		// ref. The TCP leg becomes active only when the matching HELLO_ACK is
		// accepted by Session.Ack via the OnAck closure.
		if bl.TCPRemote != "" && s.streamTransport != nil {
			ln := lane
			d := newDialer(bl.TCPRemote, s.streamTransport.Dial, func(ref transport.LegRef) {
				ln.bindTCP(ref)
				s.openLaneHello(sessionCtx, session, sessionID, ln, ln.id, ref)
				debuglog.Printf("send", "tcp_dialed session=%d lane=%d conn=%s", sessionID, ln.id, ref.ConnID)
			})
			lane.dialer = d
			go d.start(sessionCtx)
		}
	}

	s.markRunnableLanesDirty(sessionID)

	// Cold-start the bandwidth probe sweep once (spec 5.8, 7.5), if enabled. The
	// scheduler walks every lane×kind serially, gated in lockstep with the peer.
	// Uncapped client probes need the TCP reference leg in the initial snapshot,
	// so those are synchronized from the TCP HELLO_ACK path below.
	if s.startBwSchedulerAtBootstrap() {
		s.startBwScheduler(sessionCtx, sessionID)
	}
	return nil
}

// Rebootstrap tears down the current session and builds a fresh one, in response
// to an unknown-session CLOSE from the peer (spec 7: 重启→重连 自愈). It cancels
// the old session's goroutines (Hello loop, ping, dialer, bwScheduler) so the
// forgotten session is not resurrected, clears all per-session state, then runs a
// fresh bootstrap from the long-lived base context.
//
// Only the client (an end with bootstrap lanes) rebuilds — it drives the new
// HELLO handshake. A peer with no bootstrap lanes returns without acting; it
// simply waits for the rebuilt peer's fresh HELLO. Safe to call concurrently;
// only one rebuild runs at a time.
func (s *Send) Rebootstrap() error {
	if len(s.bootstrapLanes) == 0 {
		return nil // server side: nothing to drive; await the peer's new HELLO.
	}

	s.rebootstrapMu.Lock()

	baseCtx := s.getBaseContext()
	if baseCtx == nil {
		s.rebootstrapMu.Unlock()
		return nil // never bootstrapped; nothing to rebuild.
	}

	// 1+2. Tear down the old session (cancel goroutines + clear all per-session
	// state), then 3. build a fresh session from the long-lived base context.
	oldSessionID, _ := s.activeSession()
	closedTCP := s.teardownSession(oldSessionID)
	debuglog.Printf("send", "rebootstrap old_session=%d", oldSessionID)
	eventlog.Printf("reconnect", "action=rebootstrap_start old_session=%d", oldSessionID)
	err := s.bootstrapSession(baseCtx)
	if err != nil {
		eventlog.Printf("reconnect", "action=rebootstrap_error old_session=%d err=%v", oldSessionID, err)
	} else if newSessionID, ok := s.activeSession(); ok {
		eventlog.Printf("reconnect", "action=rebootstrap_done old_session=%d new_session=%d", oldSessionID, newSessionID)
	}
	s.rebootstrapMu.Unlock()
	s.closeTCPRefs(closedTCP)
	return err
}

func (s *Send) setBaseContext(ctx context.Context) {
	s.rebootMu.Lock()
	s.baseCtx = ctx
	s.rebootMu.Unlock()
}

func (s *Send) getBaseContext() context.Context {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	return s.baseCtx
}

func (s *Send) setSessionContext(ctx context.Context, cancel context.CancelFunc) {
	s.rebootMu.Lock()
	s.sessionCtx = ctx
	s.sessionCancel = cancel
	s.rebootMu.Unlock()
}

func (s *Send) getBwScheduler() *bwScheduler {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	return s.bwSched
}

func (s *Send) setBwScheduler(sched *bwScheduler) {
	s.rebootMu.Lock()
	s.bwSched = sched
	s.rebootMu.Unlock()
}

func (s *Send) qosSelectionEnabled() bool {
	if !s.enableBW {
		return true
	}
	sched := s.getBwScheduler()
	return sched != nil && sched.isDone()
}

// teardownSession cancels the current session's goroutines (Hello loop, ping,
// dialer, bwScheduler) and clears all per-session send state (sendStates, lanes,
// strategies, runnableCache, LaneManager pings/BwLoops, bwScheduler). It bounds
// memory under session churn (spec 7: 重启→重连 自愈 and plain session CLOSE).
// Idempotent: a second call for an already-cleared session is a no-op.
func (s *Send) teardownSession(sessionID uint64) []transport.LegRef {
	// Cancel the session's goroutines so stale loops cannot resurrect it.
	s.rebootMu.Lock()
	cancel := s.sessionCancel
	s.sessionCtx = nil
	s.sessionCancel = nil
	s.rebootMu.Unlock()
	if cancel != nil {
		cancel()
	}

	if active, ok := s.activeSession(); ok && active == sessionID {
		s.hasActiveSession.Store(false)
	}
	s.sessionManager.Delete(sessionID)

	s.lanesMu.Lock()
	var closedTCP []transport.LegRef
	for key := range s.lanes {
		if key.sessionID == sessionID {
			lane := s.lanes[key]
			if lane != nil {
				lane.releaseFEC()
				if ref := lane.leg.refForKind(transport.KindTCP); ref.ConnID != "" {
					closedTCP = append(closedTCP, ref)
				}
			}
			delete(s.lanes, key)
		}
	}
	s.lanesMu.Unlock()

	s.strategiesMu.Lock()
	delete(s.strategies, sessionID)
	s.strategiesMu.Unlock()

	s.sendStatesMu.Lock()
	delete(s.sendStates, sessionID)
	s.sendStatesMu.Unlock()

	s.runnableCacheMu.Lock()
	delete(s.runnableCache, sessionID)
	s.runnableCacheMu.Unlock()

	s.laneManager.Reset()
	s.setBwScheduler(nil)
	return closedTCP
}

func (s *Send) closeTCPRefs(refs []transport.LegRef) {
	if s.streamTransport == nil {
		return
	}
	for _, ref := range refs {
		_ = s.streamTransport.Close(context.Background(), ref.ConnID)
	}
}

// CloseSession tears down a session on a plain session-scope CLOSE (spec 7). It
// is the recv glue's entry for releasing send-side state when the peer closes a
// session WITHOUT the UnknownSession reason (which instead triggers Rebootstrap).
// Only acts on the session this end currently holds.
func (s *Send) CloseSession(sessionID uint64) {
	s.rebootstrapMu.Lock()
	if active, ok := s.activeSession(); !ok || active != sessionID {
		s.rebootstrapMu.Unlock()
		return
	}
	closedTCP := s.teardownSession(sessionID)
	s.rebootstrapMu.Unlock()
	s.closeTCPRefs(closedTCP)
	debuglog.Printf("send", "close_session session=%d", sessionID)
}

func (s *Send) openLaneHello(ctx context.Context, session *sessionpkg.Session, sessionID uint64, lane *laneRuntime, laneID uint8, legRef transport.LegRef) {
	if session == nil || lane == nil || legRef.Kind == 0 {
		return
	}
	sender := func(ctx context.Context, v sessionpkg.View) error {
		caps := uint16(0)
		fecProfile := protocol.FECProfileOff
		if s.FECEnabled() {
			caps = protocol.CapFEC | protocol.CapLinkStatus
			fecProfile = protocol.FECProfileSLC4Plus1
		}
		frame := protocol.Frame{
			Version:   protocol.Version,
			Type:      protocol.TypeHELLO,
			SessionID: v.SessionID(),
			LaneID:    laneID,
			Body: protocol.HelloBody{
				Nonce:      v.Nonce(),
				Caps:       caps,
				FECProfile: fecProfile,
			},
		}
		return s.WriteFrame(ctx, frame, legRef)
	}
	onExpire := func() {
		debuglog.Printf("send", "hello_expired session=%d lane=%d", sessionID, laneID)
		lane.markDown(legRef.Kind)
		s.abortBwTarget(sessionID, laneID, legRef)
		if legRef.Kind == transport.KindTCP && lane.dialer != nil {
			lane.markTCPReconnectPending()
			lane.dialer.redial()
		}
	}
	cfg := sessionpkg.HelloConfig{
		RetryInterval: 500 * time.Millisecond,
		MaxRetries:    10,
		TimeoutMS:     30000,
		OnAck: func() {
			lane.markActive(legRef.Kind)
			if legRef.Kind == transport.KindUDP && s.laneManager != nil {
				if p := s.laneManager.LookupPing(KeyForLeg(sessionID, laneID, legRef)); p != nil {
					p.MarkAlive()
				}
			}
			s.markRunnableLanesDirty(sessionID)
			s.syncBwSchedulerAfterLegActive(ctx, sessionID)
			debuglog.Printf("send", "hello_ack_active session=%d lane=%d kind=%d", sessionID, laneID, legRef.Kind)
			if legRef.Kind == transport.KindTCP && lane.consumeTCPReconnectPending() {
				eventlog.Printf("reconnect", "action=tcp_leg_reconnect_done session=%d lane=%d conn=%s",
					sessionID, laneID, legRef.ConnID)
			}
		},
	}
	_ = session.Open(ctx, cfg, sender, onExpire)
}

func (s *Send) startBwSchedulerAtBootstrap() bool {
	if !s.enableBW {
		return false
	}
	return len(s.bootstrapLanes) == 0
}

func (s *Send) syncBwSchedulerAfterLegActive(ctx context.Context, sessionID uint64) {
	if !s.enableBW || s.getBwScheduler() != nil || len(s.bootstrapLanes) == 0 {
		return
	}
	if !s.allBootstrapUDPActive(sessionID) {
		return
	}
	if s.waitBootstrapTCPForBW() && !s.allBootstrapTCPActive(sessionID) {
		return
	}
	s.startBwScheduler(ctx, sessionID)
}

func (s *Send) waitBootstrapTCPForBW() bool {
	if s.bwCapBps > 0 || s.bwReference > 0 || s.streamTransport == nil {
		return false
	}
	for _, bl := range s.bootstrapLanes {
		if bl.TCPRemote != "" {
			return true
		}
	}
	return false
}

func (s *Send) allBootstrapUDPActive(sessionID uint64) bool {
	if len(s.bootstrapLanes) == 0 {
		return false
	}
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	for _, bl := range s.bootstrapLanes {
		lane := s.lanes[laneKey{sessionID: sessionID, laneID: bl.LaneID}]
		if lane == nil || !lane.leg.isActive(transport.KindUDP) {
			return false
		}
	}
	return true
}

func (s *Send) allBootstrapTCPActive(sessionID uint64) bool {
	expectedTCP := false
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	for _, bl := range s.bootstrapLanes {
		if bl.TCPRemote == "" {
			continue
		}
		expectedTCP = true
		lane := s.lanes[laneKey{sessionID: sessionID, laneID: bl.LaneID}]
		if lane == nil || !lane.leg.isActive(transport.KindTCP) {
			return false
		}
	}
	return expectedTCP
}

// Write is the TUN DATA entry point (spec 6.1).
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	defer packet.Release()

	sessionID, ok := s.activeSession()
	if !ok {
		return nil
	}

	cost := laneScheduleCost(len(packet.Payload))

	// Pick lane using scheduler
	lane, ok := s.pickLane(sessionID, cost)
	if !ok {
		return nil
	}

	return s.sendDataFrame(ctx, sessionID, lane, packet.Payload)
}

// WriteFrame sends a control frame (spec 6.2).
func (s *Send) WriteFrame(ctx context.Context, frame protocol.Frame, to Ref) error {
	if to.Kind != 0 {
		if (to.Kind == transport.KindUDP && (to.EndpointID == "" || to.RemoteAddr == nil)) ||
			(to.Kind == transport.KindTCP && to.ConnID == "") {
			lane := s.getLane(laneKey{sessionID: frame.SessionID, laneID: frame.LaneID})
			if lane == nil {
				return ErrUnknownLane
			}
			to = lane.leg.refForKind(to.Kind)
			if to.Kind == 0 {
				return ErrLaneUnavailable
			}
		}
		if err := s.writeFrameOnLeg(ctx, to, frame); err != nil {
			return err
		}
		s.admitPassiveHelloAck(ctx, frame, to)
		return nil
	}

	// Let lane choose transport
	lane := s.getLane(laneKey{sessionID: frame.SessionID, laneID: frame.LaneID})
	if lane == nil {
		return ErrUnknownLane
	}

	return s.sendControlFrame(ctx, lane, frame)
}

// admitPassiveHelloAck creates this end's send-side lane after it has accepted
// a peer HELLO and successfully queued the corresponding HELLO_ACK. This is the
// passive server-side bootstrap path: recv glue only writes the ACK; Send owns
// the lane/runtime state derived from that accepted protocol fact.
func (s *Send) admitPassiveHelloAck(ctx context.Context, frame protocol.Frame, legRef transport.LegRef) {
	if frame.Type != protocol.TypeHELLOACK || legRef.Kind == 0 {
		return
	}
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok || body.Accepted != 1 {
		return
	}
	if _, ok := s.sessionManager.Get(frame.SessionID); !ok {
		return
	}

	s.rebootstrapMu.Lock()
	key := laneKey{sessionID: frame.SessionID, laneID: frame.LaneID}
	if active, ok := s.activeSession(); ok && active != frame.SessionID && s.getLane(key) != nil {
		s.rebootstrapMu.Unlock()
		return
	}
	sessionCtx, closedTCP := s.ensurePassiveSessionContext(ctx, frame.SessionID)

	s.sendStatesMu.Lock()
	if s.sendStates[frame.SessionID] == nil {
		s.sendStates[frame.SessionID] = &sendState{}
	}
	s.sendStatesMu.Unlock()

	lane := s.getLane(key)
	if lane == nil {
		lane = newLaneRuntime(frame.LaneID, 1)
		s.lanesMu.Lock()
		if existing := s.lanes[key]; existing != nil {
			lane = existing
		} else {
			s.lanes[key] = lane
		}
		s.lanesMu.Unlock()
	}
	s.registerLaneQoS(frame.SessionID, lane)

	switch legRef.Kind {
	case transport.KindUDP:
		lane.bindUDP(legRef)
	case transport.KindTCP:
		lane.bindTCP(legRef)
	}
	lane.markActive(legRef.Kind)
	if legRef.Kind == transport.KindUDP && s.laneManager != nil {
		if p := s.laneManager.LookupPing(KeyForLeg(frame.SessionID, frame.LaneID, legRef)); p != nil {
			p.MarkAlive()
		}
	}
	if body.Caps&protocol.CapFEC != 0 && body.FECProfile == protocol.FECProfileSLC4Plus1 {
		s.enableSessionFEC(frame.SessionID)
	}

	if legRef.Kind == transport.KindUDP && s.laneManager.LookupPing(KeyForLeg(frame.SessionID, frame.LaneID, legRef)) == nil {
		s.startLanePing(sessionCtx, frame.SessionID, lane, legRef)
	}
	s.activateSession(frame.SessionID)
	s.markRunnableLanesDirty(frame.SessionID)
	s.rebootstrapMu.Unlock()
	s.closeTCPRefs(closedTCP)
}

func (s *Send) registerLaneQoS(sessionID uint64, lane *laneRuntime) {
	if lane == nil {
		return
	}
	s.laneManager.RegisterQoS(LaneKey{SessionID: sessionID, LaneID: lane.id}, laneQoSInput{sessionID: sessionID, lane: lane})
}

func (s *Send) ensurePassiveSessionContext(ctx context.Context, sessionID uint64) (context.Context, []transport.LegRef) {
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	if active, ok := s.activeSession(); ok && active == sessionID && s.sessionCancel != nil {
		if s.sessionCtx != nil {
			return s.sessionCtx, nil
		}
		return ctx, nil
	}
	var closedTCP []transport.LegRef
	if active, ok := s.activeSession(); ok && active != sessionID {
		s.rebootMu.Unlock()
		closedTCP = s.teardownSession(active)
		s.rebootMu.Lock()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s.sessionCtx = sessionCtx
	s.sessionCancel = cancel
	return sessionCtx, closedTCP
}

// WriteTo enqueues a transport payload.
func (s *Send) WriteTo(ctx context.Context, leg Ref, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}

	packetBytes := len(packet.Payload)
	var start time.Time
	if debuglog.Enabled() {
		start = time.Now()
	}
	select {
	case s.packets <- transport.Payload{Leg: leg, Packet: packet}:
		if !start.IsZero() {
			wait := time.Since(start)
			if wait >= debugQueueWaitLogAfter {
				debuglog.Printf("send", "output_queue_wait leg={%s} wait_us=%d queue_len=%d queue_cap=%d bytes=%d",
					debugLeg(leg), wait.Microseconds(), len(s.packets), cap(s.packets), packetBytes)
			}
		}
		return nil
	case <-ctx.Done():
		packet.Release()
		return ctx.Err()
	}
}

func (s *Send) activeSession() (uint64, bool) {
	return s.activeSessionID.Load(), s.hasActiveSession.Load()
}

func (s *Send) activateSession(sessionID uint64) {
	s.activeSessionID.Store(sessionID)
	s.hasActiveSession.Store(true)
}

func (s *Send) getLane(key laneKey) *laneRuntime {
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	return s.lanes[key]
}

func (s *Send) getSendState(sessionID uint64) *sendState {
	s.sendStatesMu.RLock()
	defer s.sendStatesMu.RUnlock()
	return s.sendStates[sessionID]
}

func (s *Send) strategy(sessionID uint64) schedule.Strategy[*laneRuntime] {
	s.strategiesMu.RLock()
	strategy := s.strategies[sessionID]
	s.strategiesMu.RUnlock()

	if strategy != nil {
		return strategy
	}

	s.strategiesMu.Lock()
	defer s.strategiesMu.Unlock()

	if strategy := s.strategies[sessionID]; strategy != nil {
		return strategy
	}

	strategy = drr.New[*laneRuntime](drrBaseQuantum)
	s.strategies[sessionID] = strategy
	return strategy
}

func (s *Send) pickLane(sessionID uint64, cost uint32) (*laneRuntime, bool) {
	lanes := s.runnableLanes(sessionID)
	if len(lanes) == 0 {
		return nil, false
	}
	return s.strategy(sessionID).Pick(lanes, cost)
}

func laneScheduleCost(payloadLen int) uint32 {
	cost := payloadLen + 14 // DATA frame header + packetID
	if cost < defaultMTUBytes {
		return defaultMTUBytes
	}
	return uint32(cost)
}

func (s *Send) runnableLanes(sessionID uint64) []*laneRuntime {
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()

	var runnable []*laneRuntime
	for key, lane := range s.lanes {
		if key.sessionID == sessionID && lane.ready() {
			runnable = append(runnable, lane)
		}
	}
	return runnable
}

func (s *Send) markRunnableLanesDirty(sessionID uint64) {
	s.runnableCacheMu.Lock()
	defer s.runnableCacheMu.Unlock()
	cache := s.runnableCache[sessionID]
	if cache == nil {
		cache = &runnableCache{}
		s.runnableCache[sessionID] = cache
	}
	cache.valid = false
	cache.generation++
}

// sendDataFrame sends a DATA frame and adds to FEC window (spec 7.1).
func (s *Send) sendDataFrame(ctx context.Context, sessionID uint64, lane *laneRuntime, payload []byte) error {
	qosEnabled := s.qosSelectionEnabled()
	leg := lane.primaryTransportWithQoS(qosEnabled)
	if leg.Kind == 0 {
		return nil
	}

	fecEnabled := s.sessionFECEnabled(sessionID)
	groupID, sourceIndex, group, ready, shouldArmFlush := lane.commitPacket(payload, fecEnabled)
	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeDATA,
		SessionID: sessionID,
		LaneID:    lane.id,
		Body: protocol.DataBody{
			GroupID:     groupID,
			SourceIndex: sourceIndex,
			Packet:      payload,
		},
	}
	packet, err := s.encodeFrame(frame)
	if err != nil {
		return err
	}

	s.recordQoSDataLegSelection(sessionID, lane, leg.Kind, qosEnabled)
	if debuglog.Enabled() {
		udpQ, tcpQ := lane.leg.qualitySnapshot()
		shadow := lane.shadowTransportWithQoS(qosEnabled)
		debuglog.Printf("send", "schedule_select session=%d lane=%d primary={%s} shadow={%s} leg={%s} frame=type=DATA group_id=%d source_index=%d payload_len=%d udp_active=%t udp_qos=%t udp_qos_bps=%d udp_prefer_tcp=%t tcp_active=%t tcp_qos=%t tcp_qos_bps=%d",
			sessionID, lane.id, debugLeg(leg), debugLeg(shadow), debugLeg(leg), groupID, sourceIndex, len(payload),
			udpQ.Active, udpQ.QoSActive, udpQ.QoSDeliveredBps, udpQ.PreferTCP,
			tcpQ.Active, tcpQ.QoSActive, tcpQ.QoSDeliveredBps)
	}

	if fecEnabled {
		if ready {
			s.sendRepair(ctx, sessionID, lane, group)
		} else if shouldArmFlush {
			s.armFECFlushTimer(sessionID, lane)
		}
	}

	return s.WriteTo(ctx, leg, packet)
}

func (s *Send) qosLaneSnapshot(sessionID uint64, qosEnabled bool) string {
	s.lanesMu.RLock()
	lanes := make([]*laneRuntime, 0)
	for key, lane := range s.lanes {
		if key.sessionID == sessionID && lane != nil {
			lanes = append(lanes, lane)
		}
	}
	s.lanesMu.RUnlock()

	sort.Slice(lanes, func(i, j int) bool {
		return lanes[i].id < lanes[j].id
	})

	parts := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		primary := lane.primaryTransportWithQoS(qosEnabled)
		shadow := lane.shadowTransportWithQoS(qosEnabled)
		udpQ, tcpQ := lane.leg.qualitySnapshot()
		reason := selectorEventReason(udpQ, tcpQ, primary.Kind)
		parts = append(parts, fmt.Sprintf("%d:ready=%t,primary=%s,shadow=%s,reason=%s,repair=%d,udp_active=%t,udp_limited=%t,udp_bps=%d,udp_prefer_tcp=%t,tcp_active=%t,tcp_limited=%t,tcp_bps=%d,tcp_reconnect=%t",
			lane.id, lane.ready(),
			kindEventLabel(primary.Kind), kindEventLabel(shadow.Kind), reason,
			lane.currentFECRepairCount(),
			udpQ.Active, udpQ.QoSActive, udpQ.QoSDeliveredBps, udpQ.PreferTCP,
			tcpQ.Active, tcpQ.QoSActive, tcpQ.QoSDeliveredBps,
			lane.tcpReconnectPending.Load()))
	}
	return strings.Join(parts, ";")
}

func (s *Send) recordQoSDataLegSelection(sessionID uint64, lane *laneRuntime, kind transport.Kind, qosEnabled bool) {
	if lane == nil || kind == 0 {
		return
	}
	if !qosEnabled {
		return
	}
	udpQ, tcpQ := lane.leg.qualitySnapshot()
	qosActive := udpQ.QoSActive || tcpQ.QoSActive
	previousKind, changed := lane.leg.noteQoSSelectedPrimary(kind, qosActive)
	if !changed {
		return
	}
	eventlog.Printf("qos", "action=selector session=%d lane=%d from=%s to=%s reason=%s qos_enabled=%t lanes=%s",
		sessionID, lane.id, kindEventLabel(previousKind), kindEventLabel(kind),
		selectorEventReason(udpQ, tcpQ, kind), qosEnabled,
		s.qosLaneSnapshot(sessionID, qosEnabled))
}

func selectorEventReason(udpQ, tcpQ selector.Quality, selected transport.Kind) string {
	switch {
	case udpQ.Active && !tcpQ.Active:
		return "udp_only_active"
	case !udpQ.Active && tcpQ.Active:
		return "tcp_only_active"
	case udpQ.QoSActive && !tcpQ.QoSActive:
		if selected == transport.KindTCP {
			return "qos_avoid_udp"
		}
		return "qos_udp_limited_selected"
	case !udpQ.QoSActive && tcpQ.QoSActive:
		if selected == transport.KindUDP {
			return "qos_avoid_tcp"
		}
		return "qos_tcp_limited_selected"
	case udpQ.QoSActive && tcpQ.QoSActive:
		if selected == transport.KindTCP {
			return "qos_bps_tcp"
		}
		return "qos_bps_udp"
	case udpQ.PreferTCP && selected == transport.KindTCP:
		return "prefer_tcp"
	case selected == transport.KindUDP:
		return "default_udp"
	case selected == transport.KindTCP:
		return "selected_tcp"
	default:
		return "unknown"
	}
}

func (s *Send) logBandwidthProbeDecision(target bwTarget, sample bw.Sample, preferTCP bool) {
	if target.kind != transport.KindUDP {
		return
	}
	referenceBps := target.referenceBps
	tcpBps := uint64(0)
	if sched := s.getBwScheduler(); sched != nil {
		tcpBps = sched.tcpReferenceBps(target.key.SessionID, target.laneID)
	}
	if referenceBps == 0 {
		referenceBps = tcpBps
	}
	if referenceBps == 0 {
		return
	}
	lane := s.getLane(laneKey{sessionID: target.key.SessionID, laneID: target.laneID})
	if lane == nil {
		return
	}
	udpQ, tcpQ := lane.leg.qualitySnapshot()
	selected := "udp"
	if preferTCP && tcpQ.Active {
		selected = "tcp"
	}
	tcpBetter := referenceBps > sample.BandwidthBps
	eventlog.Printf("bandwidth_probe_decision", "side=send session=%d lane=%d udp_active=%t tcp_active=%t udp_bps=%d udp_loss=%.3f tcp_bps=%d reference_bps=%d cap_bps=%d udp_qos_limited=%t tcp_better=%t prefer_tcp=%t selected_leg=%s",
		target.key.SessionID, target.laneID,
		udpQ.Active, tcpQ.Active,
		sample.BandwidthBps, sample.Loss,
		tcpBps, referenceBps, target.capBps,
		udpQ.QoSActive, tcpBetter, preferTCP, selected)
}

func (s *Send) sendControlFrame(ctx context.Context, lane *laneRuntime, frame protocol.Frame) error {
	packet, err := s.encodeFrame(frame)
	if err != nil {
		return err
	}

	leg := lane.chooseControlTransportWithQoS(s.qosSelectionEnabled())
	if leg.Kind == 0 {
		packet.Release()
		return ErrLaneUnavailable
	}

	return s.WriteTo(ctx, leg, packet)
}

func (s *Send) writeFrameOnLeg(ctx context.Context, leg Ref, frame protocol.Frame) error {
	packet, err := s.encodeFrame(frame)
	if err != nil {
		return err
	}

	return s.WriteTo(ctx, leg, packet)
}

func (s *Send) encodeFrame(frame protocol.Frame) (*packetbuf.Packet, error) {
	size := estimateFrameSize(frame)
	packet := packetbuf.Acquire(size)

	encoded, err := protocol.Encode(frame, packet.Payload[:0])
	if err != nil {
		packet.Release()
		return nil, err
	}

	packet.Payload = encoded
	return packet, nil
}

func estimateFrameSize(frame protocol.Frame) int {
	const headerSize = 10
	switch frame.Type {
	case protocol.TypeHELLO:
		return headerSize + 11
	case protocol.TypeHELLOACK:
		return headerSize + 12
	case protocol.TypePING, protocol.TypePONG:
		return headerSize + 16
	case protocol.TypeDATA:
		body := frame.Body.(protocol.DataBody)
		return headerSize + 4 + len(body.Packet)
	case protocol.TypeREPAIR:
		body := frame.Body.(protocol.RepairBody)
		return headerSize + 7 + len(body.Symbol)
	case protocol.TypeCLOSE:
		return headerSize + 2
	case protocol.TypeBandwidthProbe:
		body := frame.Body.(protocol.BandwidthProbeBody)
		return headerSize + 44 + len(body.Payload)
	case protocol.TypeBandwidthProbeAck:
		return headerSize + 36
	default:
		return headerSize + 256
	}
}

// sendRepair sends REPAIR frames (spec 7.2).
func (s *Send) sendRepair(ctx context.Context, sessionID uint64, lane *laneRuntime, group txRepairGroup) {
	if len(group.packets) == 0 {
		return
	}

	state := s.getSendState(sessionID)
	if state == nil {
		for _, pkt := range group.packets {
			pkt.Release()
		}
		return
	}

	sourceSpan := int(group.sourceSpan)
	configuredRepairCount := lane.currentFECRepairCount()
	repairCount := scaledFECRepairCount(configuredRepairCount, sourceSpan)
	if repairCount == 0 {
		for _, pkt := range group.packets {
			pkt.Release()
		}
		return
	}
	keys := make([]uint16, repairCount)
	for i := range keys {
		keys[i] = uint16(state.nextRepairKey.Add(1) - 1)
	}

	// Encode FEC
	codec := s.fecCodecFor(sourceSpan, repairCount)
	if codec == nil {
		for _, pkt := range group.packets {
			pkt.Release()
		}
		return
	}

	shards := make([][]byte, sourceSpan+int(repairCount))
	for i, pkt := range group.packets {
		shards[i] = pkt.Payload
	}

	if err := codec.Encode(shards, keys); err != nil {
		debuglog.Printf("send/fec", "encode_err session=%d lane=%d err=%v", sessionID, lane.id, err)
		for _, pkt := range group.packets {
			pkt.Release()
		}
		return
	}

	// Release DATA packets
	for _, pkt := range group.packets {
		pkt.Release()
	}

	// Send REPAIR on shadow transport
	qosEnabled := s.qosSelectionEnabled()
	leg := lane.shadowTransportWithQoS(qosEnabled)
	if leg.Kind == 0 {
		return
	}
	debuglog.Printf("send/fec", "repair_group session=%d lane=%d group_id=%d source_span=%d packet_count=%d configured_repair_count=%d scaled_repair_count=%d leg={%s}",
		sessionID, lane.id, group.groupID, group.sourceSpan, len(group.packets), configuredRepairCount, repairCount, debugLeg(leg))

	for i, key := range keys {
		symbol := shards[sourceSpan+i]
		frame := protocol.Frame{
			Version:   protocol.Version,
			Type:      protocol.TypeREPAIR,
			SessionID: sessionID,
			LaneID:    lane.id,
			Body: protocol.RepairBody{
				GroupID:     group.groupID,
				Key:         key,
				SourceSpan:  group.sourceSpan,
				RepairCount: repairCount,
				Symbol:      symbol,
			},
		}

		packet, err := s.encodeFrame(frame)
		if err != nil {
			return
		}

		if debuglog.Enabled() {
			primary := lane.primaryTransportWithQoS(qosEnabled)
			debuglog.Printf("send", "schedule_select session=%d lane=%d primary={%s} shadow={%s} leg={%s} frame=type=REPAIR group_id=%d key=%d source_span=%d symbol_len=%d",
				sessionID, lane.id, debugLeg(primary), debugLeg(leg), debugLeg(leg),
				group.groupID, key, group.sourceSpan, len(symbol))
		}

		_ = s.WriteTo(ctx, leg, packet)
	}
}

func scaledFECRepairCount(repairCount uint8, sourceSpan int) uint8 {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return 0
	}
	if repairCount == 0 {
		repairCount = 1
	}
	if repairCount > maxFECSourceSpan {
		repairCount = maxFECSourceSpan
	}
	scaled := (int(repairCount)*sourceSpan + maxFECSourceSpan - 1) / maxFECSourceSpan
	if scaled < 1 {
		return 1
	}
	if scaled > sourceSpan {
		return uint8(sourceSpan)
	}
	return uint8(scaled)
}

func (s *Send) fecCodecFor(sourceSpan int, repairCount uint8) *fec.Codec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan || repairCount == 0 || repairCount > 4 {
		return nil
	}
	return s.fecCodecs[sourceSpan][repairCount]
}

func (s *Send) armFECFlushTimer(sessionID uint64, lane *laneRuntime) {
	d := s.fecFlushMin

	lane.fecMu.Lock()
	defer lane.fecMu.Unlock()

	if lane.txWindow == nil || len(lane.txWindow.pending) == 0 {
		return
	}

	if lane.fecFlushTimer == nil {
		lane.fecFlushTimer = time.AfterFunc(d, func() {
			s.handleFECFlush(sessionID, lane)
		})
	} else {
		lane.fecFlushTimer.Reset(d)
	}
	lane.fecFlushArmed = true
}

func (s *Send) handleFECFlush(sessionID uint64, lane *laneRuntime) {
	if !s.sessionFECEnabled(sessionID) {
		return
	}

	lane.fecMu.Lock()
	lane.fecFlushArmed = false
	group, ready := lane.txWindow.flush()
	lane.fecMu.Unlock()

	if !ready {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.sendRepair(ctx, sessionID, lane, group)
}

// OnLegFailure implements transport.LegFailureHandler (spec 6.3). It is called
// by the stream transport when TCP write/read fails. TCP liveness is driven by
// I/O errors, not ping (spec: TCP不靠probe判死, 只靠tcp dial/write/read error).
// This marks the TCP leg down and triggers the dialer to re-establish the conn.
func (s *Send) OnLegFailure(ctx context.Context, legRef transport.LegRef, err error) {
	if legRef.Kind != transport.KindTCP {
		return // Only TCP legs report I/O failures; UDP liveness is ping-driven.
	}

	// Find the lane holding this TCP connID and mark TCP down.
	s.lanesMu.RLock()
	type affectedLane struct {
		sessionID uint64
		lane      *laneRuntime
	}
	var affected []affectedLane
	for key, lane := range s.lanes {
		if ref := lane.leg.refForKind(transport.KindTCP); ref.ConnID == legRef.ConnID {
			affected = append(affected, affectedLane{sessionID: key.sessionID, lane: lane})
		}
	}
	s.lanesMu.RUnlock()

	for _, affected := range affected {
		affected.lane.markDown(transport.KindTCP)
		s.abortBwTarget(affected.sessionID, affected.lane.id, legRef)
		if affected.lane.dialer != nil {
			affected.lane.markTCPReconnectPending()
			affected.lane.dialer.redial()
		}
		debuglog.Printf("send", "tcp_leg_failure conn=%s err=%v", legRef.ConnID, err)
		eventlog.Printf("reconnect", "action=tcp_leg_failure session=%d lane=%d conn=%s err=%v",
			affected.sessionID, affected.lane.id, legRef.ConnID, err)
	}
}

// startLanePing creates and drives the active ping for one lane transport path
// (spec 5.5, 7.3). The ping starts dead when the leg is still inactive; its
// first RecoverSuccess pongs fire OnUp → leg.markActive. If HELLO_ACK already
// activated the same leg, the ping starts alive so later MaxLoss timeouts can
// still fire OnDown → leg.markDown.
//
// The closures capture this lane's leg, so liveness lands on leg.markActive /
// leg.markDown entirely inside send — no transport method is exposed. The ping
// is registered in the shared LaneManager keyed by leg so the recv glue can
// route inbound PONG to it (LookupPing) without touching send.
//
// The ping's sendMsg closure converts each ping.Message to a PING frame and
// writes it on the exact transport via WriteFrame. The Observer closure feeds
// each RTT sample into the lane's leg observer for selector quality.
func (s *Send) startLanePing(ctx context.Context, sessionID uint64, lane *laneRuntime, legRef transport.LegRef) {
	if s.laneManager == nil {
		return
	}
	if legRef.Kind != transport.KindUDP {
		return
	}

	laneID := lane.id
	kind := legRef.Kind
	initDead := !lane.leg.isActive(kind)

	sendMsg := func(m ping.Message) error {
		frame := protocol.Frame{
			Version:   protocol.Version,
			Type:      protocol.TypePING,
			SessionID: sessionID,
			LaneID:    laneID,
			Body: protocol.PingBody{
				PingID: m.ID,
				TimeMS: m.TimeMS,
			},
		}
		return s.WriteFrame(ctx, frame, legRef)
	}

	p := ping.New(ping.Config{
		Interval: s.probeInterval,
		Timeout:  s.probeTimeout,
		SendMsg:  sendMsg,
		InitDead: initDead,
		OnUp: func() {
			lane.markActive(kind)
			debuglog.Printf("send", "ping_up session=%d lane=%d kind=%d", sessionID, laneID, kind)
			eventlog.Printf("ping", "action=up session=%d lane=%d leg=%s",
				sessionID, laneID, kindEventLabel(kind))
		},
		OnDown: func() {
			lane.markDown(kind)
			s.abortBwTarget(sessionID, laneID, legRef)
			debuglog.Printf("send", "ping_down session=%d lane=%d kind=%d", sessionID, laneID, kind)
			eventlog.Printf("ping", "action=down session=%d lane=%d leg=%s",
				sessionID, laneID, kindEventLabel(kind))
		},
		Observer: func(q ping.Quality) {
			lane.leg.observeRTT(kind, q.SampleMS)
		},
		OnDelivery: func(onTime bool) {
			lane.leg.observeDelivery(kind, onTime)
		},
	})

	key := KeyForLeg(sessionID, laneID, legRef)
	if !s.laneManager.registerPingIfAbsent(key, p) {
		return
	}

	go func() {
		if err := p.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			debuglog.Printf("send", "ping_stop session=%d lane=%d kind=%d err=%v", sessionID, laneID, kind, err)
		}
	}()
}

func (s *Send) abortBwTarget(sessionID uint64, laneID uint8, legRef transport.LegRef) {
	if legRef.Kind == 0 {
		return
	}
	if sched := s.getBwScheduler(); sched != nil {
		sched.abort(KeyForLeg(sessionID, laneID, legRef))
	}
}

// bwPreferTCPThresholds: a UDP probe sample locks PreferTCP when loss is high or
// the UDP bandwidth falls well short of the TCP reference (spec 5.4/5.8 cold-start
// lock). Mirrors the old qosLimited/preferTCP rule, trimmed.
const (
	bwLossThreshold  = 0.01
	bwTCPPreferRatio = 3.0 / 2.0
)

// bwPreferTCP decides the probeBW cold-start PreferTCP lock from a UDP sample.
func bwPreferTCP(sample bw.Sample) bool {
	if sample.Loss >= bwLossThreshold {
		return true
	}
	if sample.ReferenceBps > 0 && float64(sample.BandwidthBps)*bwTCPPreferRatio <= float64(sample.ReferenceBps) {
		return true
	}
	return false
}

// Per-kind bandwidth probe payload + stop tuning (spec: UDP randomizes
// 1200-1400 and stops early on loss; TCP fixes a large payload and relies on the
// plateau check, never stopping on loss). The bw package stays zero-identity —
// these raw values are injected via bw.Config, bw never sees the kind.
const (
	bwUDPPayloadMin   = 1200
	bwUDPPayloadMax   = 1400
	bwTCPPayloadFixed = 32 * 1024
	bwUDPLossStop     = 0.01
)

// bwKindTuning returns the payload bounds and loss-stop for a transport kind.
func bwKindTuning(kind transport.Kind) (payloadMin, payloadMax int, lossStop float64, rateLimit bool) {
	if kind == transport.KindTCP {
		return bwTCPPayloadFixed, bwTCPPayloadFixed, 0, false // fixed payload, no loss stop
	}
	return bwUDPPayloadMin, bwUDPPayloadMax, bwUDPLossStop, true // UDP: randomize + loss stop + pace
}

// startBwScheduler creates and cold-starts the bandwidth probe sweep (spec 5.8,
// 7.5). Send stays thin: it only hands the scheduler isClient + capBps + a lanes
// snapshot + the closures. The closures own the protocol/transport wiring:
//   - newLoop creates a bw.BW for the target leg (SendProbe → WriteFrame BW_PROBE;
//     OnSample → setPreferTCP + DeleteBwLoop + advanceAfterLocal), starts a
//     BwLoop, and stores it in the LaneManager so inbound BW_ACK can reach it.
//   - remoteComplete (registered on the LaneManager) advances the gate when the
//     recv glue reports the peer's train done (BW_PROBE remaining==0).
//
// bw failure never marks the lane down (spec 5.8): lane health is ping's job.
func (s *Send) startBwScheduler(ctx context.Context, sessionID uint64) {
	if !s.enableBW {
		return
	}
	s.rebootMu.Lock()
	if s.bwSched != nil {
		s.rebootMu.Unlock()
		return
	}
	s.rebootMu.Unlock()

	snapshot := func() []bwTarget {
		s.lanesMu.RLock()
		defer s.lanesMu.RUnlock()
		return buildBwTargets(sessionID, s.lanes, s.bwCapBps)
	}

	newLoop := func(t bwTarget) *bw.BwLoop {
		lane := s.getLane(laneKey{sessionID: sessionID, laneID: t.laneID})
		if lane == nil {
			return nil
		}
		var loopPtr atomic.Pointer[bw.BwLoop]
		payloadMin, payloadMax, lossStop, rateLimit := bwKindTuning(t.kind)
		b := bw.New(bw.Config{
			ReferenceBps:      t.referenceBps,
			CapBps:            t.capBps,
			PayloadMin:        payloadMin,
			PayloadMax:        payloadMax,
			LossStopThreshold: lossStop,
			RateLimit:         rateLimit,
			SendProbe: func(p bw.Probe) error {
				frame := protocol.Frame{
					Version:   protocol.Version,
					Type:      protocol.TypeBandwidthProbe,
					SessionID: sessionID,
					LaneID:    t.laneID,
					Body: protocol.BandwidthProbeBody{
						TrainID:             p.TrainID,
						ProbeID:             p.ID,
						Seq:                 p.Seq,
						Count:               p.Count,
						SendMS:              p.SendMS,
						TargetBps:           p.TargetBps,
						TrainBytesRemaining: p.Remaining,
						Payload:             make([]byte, p.Bytes),
					},
				}
				return s.WriteFrame(ctx, frame, t.leg)
			},
			OnSample: func(sample bw.Sample) {
				preferTCP := t.kind == transport.KindUDP && bwPreferTCP(sample)
				if t.kind == transport.KindUDP {
					lane.leg.setPreferTCP(preferTCP)
				}
				if l := loopPtr.Load(); l != nil {
					s.laneManager.DeleteBwLoop(l.TrainID())
				}
				if sched := s.getBwScheduler(); sched != nil {
					sched.completeLocal(t, sample)
				}
				debuglog.Printf("send/bw", "sample session=%d lane=%d kind=%d bps=%d loss=%.3f",
					sessionID, t.laneID, t.kind, sample.BandwidthBps, sample.Loss)
				eventlog.Printf("bw", "action=sample session=%d lane=%d leg=%s bps=%d loss=%.3f reference_bps=%d cap_bps=%d prefer_tcp=%t",
					sessionID, t.laneID, kindEventLabel(t.kind), sample.BandwidthBps, sample.Loss,
					t.referenceBps, t.capBps, preferTCP)
				s.logBandwidthProbeDecision(t, sample, preferTCP)
			},
		})
		loop, err := b.Start(ctx)
		if err != nil || loop == nil {
			return nil
		}
		loopPtr.Store(loop)
		s.laneManager.PutBwLoop(loop.TrainID(), loop)
		return loop
	}

	sched := newBwScheduler(s.isClient, s.bwCapBps, s.bwReference, snapshot, newLoop)
	s.rebootMu.Lock()
	if s.bwSched != nil {
		s.rebootMu.Unlock()
		return
	}
	s.bwSched = sched
	eventlog.Printf("bw", "action=scheduler_start session=%d is_client=%t cap_bps=%d reference_bps=%d",
		sessionID, s.isClient, s.bwCapBps, s.bwReference)
	s.laneManager.SetRemoteComplete(func(key LegKey) {
		if sched := s.getBwScheduler(); sched != nil {
			sched.advanceAfterRemote(key)
		}
	})
	s.rebootMu.Unlock()
	go sched.Start(ctx)
}

func (s *Send) installBandwidthRemoteProbe() {
	if s.laneManager == nil {
		return
	}
	s.laneManager.SetRemoteProbe(func(key LegKey) {
		if !s.enableBW || s.isClient {
			return
		}
		ctx, ok := s.currentSessionContext(key.SessionID)
		if !ok {
			return
		}
		s.startBwScheduler(ctx, key.SessionID)
	})
}

func (s *Send) currentSessionContext(sessionID uint64) (context.Context, bool) {
	if active, ok := s.activeSession(); !ok || active != sessionID {
		return nil, false
	}
	s.rebootMu.Lock()
	defer s.rebootMu.Unlock()
	if s.sessionCtx == nil {
		return nil, false
	}
	return s.sessionCtx, true
}

func kindEventLabel(kind transport.Kind) string {
	switch kind {
	case transport.KindUDP:
		return "udp"
	case transport.KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}
