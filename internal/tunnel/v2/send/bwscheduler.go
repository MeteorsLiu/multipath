package send

import (
	"context"
	"sort"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
)

// bwGatePhase is which side currently holds the probe gate for a target
// (spec 7.5). Local = this end probes; Remote = this end waits for the peer.
type bwGatePhase uint8

const (
	bwPhaseLocal bwGatePhase = iota
	bwPhaseRemote
)

const defaultBandwidthReferenceBps = uint64(200_000_000)

// bwTarget is one (lane, transport-kind) cell the gate visits (spec 7.5). The
// gate walks targets in a fixed order: per lane (ascending laneID) TCP then UDP,
// or UDP only when capBps>0.
type bwTarget struct {
	laneID       uint8
	kind         transport.Kind
	leg          transport.LegRef
	key          LegKey
	referenceBps uint64
	capBps       uint64
}

// bwScheduler serializes bandwidth probing across all lanes (spec 5.8, 7.5).
// Bandwidth can only be measured by saturating the link, so at most one end, one
// lane, one kind probes at a time. The scheduler holds a local gate; the two ends
// stay in lockstep via the probe-frame exchange (齿轮咬合), not shared state.
//
// It is coarse and self-contained (plan 5.8): it owns all gate logic. Send stays
// thin — it only hands the scheduler isClient, a lanes view, and the closures
// (newLoop creates a BwLoop and stores it in the LaneManager; onSample feeds the
// observer's PreferTCP). The scheduler never marks a lane down (bw failure ≠
// lane death; lane health is ping's job, spec 5.8).
type bwScheduler struct {
	isClient     bool
	capBps       uint64
	referenceBps uint64

	// snapshot returns the current ordered probe targets (cold-start once).
	snapshot func() []bwTarget
	// newLoop creates + starts a BwLoop for the target leg and stores it in the
	// LaneManager (so inbound BW_PROBE_ACK can reach it). Returns nil on failure.
	newLoop func(t bwTarget) *bw.BwLoop
	mu      sync.Mutex
	targets []bwTarget
	idx     int
	phase   bwGatePhase
	current *bw.BwLoop // active local loop (for abort)
	tcpRef  map[LaneKey]uint64
	wake    chan struct{}
	started bool
	done    bool
}

func newBwScheduler(isClient bool, capBps, referenceBps uint64, snapshot func() []bwTarget, newLoop func(bwTarget) *bw.BwLoop) *bwScheduler {
	return &bwScheduler{
		isClient:     isClient,
		capBps:       capBps,
		referenceBps: referenceBps,
		snapshot:     snapshot,
		newLoop:      newLoop,
		tcpRef:       make(map[LaneKey]uint64),
		wake:         make(chan struct{}, 1),
	}
}

// Start runs the cold-start probe sweep once: it walks every target, alternating
// local/remote phases with the peer, until all targets are measured or ctx ends.
// Client starts phase=Local (probes first); server starts phase=Remote (waits
// first). Run as `go sched.Start(ctx)`.
func (s *bwScheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.targets = s.snapshot()
	s.idx = 0
	if s.isClient {
		s.phase = bwPhaseLocal
	} else {
		s.phase = bwPhaseRemote
	}
	s.mu.Unlock()

	s.eval(ctx)
}

// eval is the gate-driven loop. It launches a local probe when the gate enters a
// Local phase, and waits when it is Remote. Advances are signaled via wake by
// advanceAfterLocal/advanceAfterRemote/abort.
func (s *bwScheduler) eval(ctx context.Context) {
	for {
		s.mu.Lock()
		if s.done || s.idx >= len(s.targets) {
			s.done = true
			s.mu.Unlock()
			return
		}
		target := s.targets[s.idx]
		phase := s.phase
		s.mu.Unlock()

		switch phase {
		case bwPhaseLocal:
			s.runLocal(ctx, target)
			// runLocal blocks until the local train finishes (onSample →
			// advanceAfterLocal) or ctx/ abort. The gate has advanced by the time
			// we loop; just re-evaluate.
			if !s.waitWake(ctx) {
				return
			}
		case bwPhaseRemote:
			// Wait for the peer to finish probing this target. Transport or ping
			// failures abort the scheduler path explicitly; the remote phase does
			// not self-advance on a short timer.
			if !s.waitWake(ctx) {
				return
			}
		}
	}
}

// runLocal launches the local probe train for target and records it for abort.
func (s *bwScheduler) runLocal(ctx context.Context, target bwTarget) {
	target = s.prepareTarget(target)
	loop := s.newLoop(target)
	s.mu.Lock()
	s.current = loop
	s.mu.Unlock()
	if loop == nil {
		// Could not start (e.g. leg not active): skip this local phase by
		// self-advancing as if the local probe completed.
		debuglog.Printf("send/bw", "gate_local_skip lane=%d kind=%d", target.laneID, target.kind)
		eventlog.Printf("bw", "action=local_skip session=%d lane=%d leg=%s reference_bps=%d cap_bps=%d",
			target.key.SessionID, target.laneID, kindEventLabel(target.kind), target.referenceBps, target.capBps)
		go s.advanceAfterLocal(target.key)
	}
}

func (s *bwScheduler) prepareTarget(target bwTarget) bwTarget {
	if target.kind != transport.KindUDP {
		target.referenceBps = s.referenceBps
		if target.referenceBps == 0 {
			target.referenceBps = defaultBandwidthReferenceBps
		}
		target.capBps = 0
		return target
	}
	if s.referenceBps > 0 {
		target.referenceBps = s.referenceBps
		target.capBps = s.capBps
		return target
	}
	if s.capBps > 0 {
		target.referenceBps = s.capBps
		target.capBps = s.capBps
		return target
	}
	s.mu.Lock()
	ref := s.tcpRef[LaneKey{SessionID: target.key.SessionID, LaneID: target.laneID}]
	s.mu.Unlock()
	target.referenceBps = ref
	target.capBps = ref
	return target
}

func (s *bwScheduler) completeLocal(target bwTarget, sample bw.Sample) {
	s.mu.Lock()
	if s.done || s.idx >= len(s.targets) || s.phase != bwPhaseLocal || s.targets[s.idx].key != target.key {
		s.mu.Unlock()
		return
	}
	if target.kind == transport.KindTCP && sample.BandwidthBps > 0 {
		s.tcpRef[LaneKey{SessionID: target.key.SessionID, LaneID: target.laneID}] = sample.BandwidthBps
	}
	s.current = nil
	if s.isClient {
		s.phase = bwPhaseRemote
	} else {
		s.idx++
		s.phase = bwPhaseRemote
	}
	s.mu.Unlock()
	s.signal()
}

func (s *bwScheduler) tcpReferenceBps(sessionID uint64, laneID uint8) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpRef[LaneKey{SessionID: sessionID, LaneID: laneID}]
}

// waitWake blocks until a gate advance is signaled or ctx ends. Returns false
// if ctx ended.
func (s *bwScheduler) waitWake(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	}
}

// advanceAfterLocal moves the gate after this end finishes probing a target
// (spec 7.5: onSample → advanceAfterLocal). Client: Local→Remote (same target).
// Server: Local→Remote and step to the next target. No-op if the gate has moved.
func (s *bwScheduler) advanceAfterLocal(key LegKey) {
	s.mu.Lock()
	if s.done || s.idx >= len(s.targets) || s.phase != bwPhaseLocal || s.targets[s.idx].key != key {
		s.mu.Unlock()
		return
	}
	s.current = nil
	if s.isClient {
		s.phase = bwPhaseRemote
	} else {
		s.idx++
		s.phase = bwPhaseRemote
	}
	s.mu.Unlock()
	s.signal()
}

// advanceAfterRemote moves the gate after the peer finishes probing a target
// (spec 7.5: OnBW_PROBE remaining==0 → advanceAfterRemote). Client: Remote→Local
// and step to the next target. Server: Remote→Local (same target). No-op if the
// gate has moved.
func (s *bwScheduler) advanceAfterRemote(key LegKey) {
	s.mu.Lock()
	if s.done || s.idx >= len(s.targets) || s.phase != bwPhaseRemote || s.targets[s.idx].key != key {
		s.mu.Unlock()
		return
	}
	if s.isClient {
		s.idx++
		s.phase = bwPhaseLocal
	} else {
		s.phase = bwPhaseLocal
	}
	s.mu.Unlock()
	s.signal()
}

// abort stops the active local train for key and releases the gate (spec 7.5:
// leg 死 → 中止 train + 释放 gate). bw failure does not mark the lane down. It
// advances as if the local probe for that target completed.
func (s *bwScheduler) abort(key LegKey) {
	s.mu.Lock()
	if s.done || s.idx >= len(s.targets) || s.targets[s.idx].key != key {
		s.mu.Unlock()
		return
	}
	loop := s.current
	s.current = nil
	phase := s.phase
	s.mu.Unlock()

	if loop != nil {
		loop.Stop()
	}
	debuglog.Printf("send/bw", "gate_abort lane=%d kind=%d", key.LaneID, key.Kind)
	eventlog.Printf("bw", "action=gate_abort session=%d lane=%d leg=%s", key.SessionID, key.LaneID, kindEventLabel(key.Kind))

	// Release the gate by advancing past the current phase.
	if phase == bwPhaseLocal {
		s.advanceAfterLocal(key)
	} else {
		s.advanceAfterRemote(key)
	}
}

func (s *bwScheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// buildBwTargets returns the ordered probe targets for the given lanes: per lane
// (ascending laneID) TCP then UDP, or UDP only when capBps>0 (spec 7.5). Only
// legs with a usable ref are included.
func buildBwTargets(sessionID uint64, lanes map[laneKey]*laneRuntime, capBps uint64) []bwTarget {
	type laneEntry struct {
		id   uint8
		lane *laneRuntime
	}
	var entries []laneEntry
	for key, lane := range lanes {
		if key.sessionID == sessionID {
			entries = append(entries, laneEntry{id: key.laneID, lane: lane})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	var targets []bwTarget
	for _, e := range entries {
		kinds := []transport.Kind{transport.KindTCP, transport.KindUDP}
		if capBps > 0 {
			kinds = []transport.Kind{transport.KindUDP} // capBps>0 跳过 TCP
		}
		for _, kind := range kinds {
			ref := e.lane.leg.refForKind(kind)
			if ref.Kind == 0 {
				continue // no usable leg for this kind
			}
			targets = append(targets, bwTarget{
				laneID: e.id,
				kind:   kind,
				leg:    ref,
				key:    KeyForLeg(sessionID, e.id, ref),
			})
		}
	}
	return targets
}
