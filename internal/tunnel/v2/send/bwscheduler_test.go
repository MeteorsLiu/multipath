package send

import (
	"context"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
)

// gateTestTargets builds N synthetic targets (no real legs needed for the pure
// gate state-machine test).
func gateTestTargets(n int) []bwTarget {
	targets := make([]bwTarget, 0, n)
	for i := 0; i < n; i++ {
		laneID := uint8(i + 1)
		ref := transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep"}
		targets = append(targets, bwTarget{
			laneID: laneID,
			kind:   transport.KindUDP,
			leg:    ref,
			key:    LegKey{SessionID: 1, LaneID: laneID, Kind: transport.KindUDP, Endpoint: "ep"},
		})
	}
	return targets
}

// newGateOnly builds a scheduler with targets pre-installed for direct gate
// state-machine testing (no Start goroutine, no real loops).
func newGateOnly(isClient bool, targets []bwTarget) *bwScheduler {
	s := newBwScheduler(isClient, 0, 0, func() []bwTarget { return targets }, nil, nil)
	s.targets = targets
	s.idx = 0
	if isClient {
		s.phase = bwPhaseLocal
	} else {
		s.phase = bwPhaseRemote
	}
	s.started = true
	// Drain the wake channel after each advance so we don't block.
	return s
}

func (s *bwScheduler) snapshotState() (idx int, phase bwGatePhase, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idx, s.phase, s.done
}

func (s *bwScheduler) drainWake() {
	select {
	case <-s.wake:
	default:
	}
}

// TestGateClientServerLockstep simulates the gear-mesh between a client and a
// server gate over the same target list, driving each end's advance the moment
// its counterpart's phase completes, and asserts both ends probe every target
// exactly once in lockstep (spec 7.5: 齿轮咬合).
func TestGateClientServerLockstep(t *testing.T) {
	const n = 3
	targets := gateTestTargets(n)
	client := newGateOnly(true, targets)
	server := newGateOnly(false, targets)

	// Initial: client Local on target 0, server Remote on target 0.
	if idx, ph, _ := client.snapshotState(); idx != 0 || ph != bwPhaseLocal {
		t.Fatalf("client init = (%d,%d), want (0,Local)", idx, ph)
	}
	if idx, ph, _ := server.snapshotState(); idx != 0 || ph != bwPhaseRemote {
		t.Fatalf("server init = (%d,%d), want (0,Remote)", idx, ph)
	}

	localProbesClient := map[uint8]int{}
	localProbesServer := map[uint8]int{}

	// The gear turns in this repeating order for each target:
	//   1. client probes locally (Local on target i).
	//   2. client finishes → advanceAfterLocal: client Remote(i).
	//      The peer (server) observed the client's probe complete → server
	//      advanceAfterRemote: server Local(i).
	//   3. server probes locally (Local on target i).
	//   4. server finishes → advanceAfterLocal: server Remote(i+1).
	//      The peer (client) observed the server's probe complete → client
	//      advanceAfterRemote: client Local(i+1).
	for i := 0; i < n; i++ {
		laneID := uint8(i + 1)
		key := LegKey{SessionID: 1, LaneID: laneID, Kind: transport.KindUDP, Endpoint: "ep"}

		// Step 1-2: client local on target i.
		if idx, ph, _ := client.snapshotState(); idx != i || ph != bwPhaseLocal {
			t.Fatalf("target %d: client = (%d,%d), want (%d,Local)", i, idx, ph, i)
		}
		localProbesClient[laneID]++
		client.advanceAfterLocal(key) // client: Local→Remote(i)
		client.drainWake()
		server.advanceAfterRemote(key) // server saw client's probe: Remote→Local(i)
		server.drainWake()

		if idx, ph, _ := client.snapshotState(); idx != i || ph != bwPhaseRemote {
			t.Fatalf("target %d: client after local = (%d,%d), want (%d,Remote)", i, idx, ph, i)
		}
		if idx, ph, _ := server.snapshotState(); idx != i || ph != bwPhaseLocal {
			t.Fatalf("target %d: server after remote = (%d,%d), want (%d,Local)", i, idx, ph, i)
		}

		// Step 3-4: server local on target i.
		localProbesServer[laneID]++
		server.advanceAfterLocal(key) // server: Local→Remote, idx++
		server.drainWake()
		client.advanceAfterRemote(key) // client saw server's probe: Remote→Local, idx++
		client.drainWake()

		// Both ends should now point at target i+1.
		wantIdx := i + 1
		if idx, _, _ := client.snapshotState(); idx != wantIdx {
			t.Fatalf("target %d: client idx after pair = %d, want %d", i, idx, wantIdx)
		}
		if idx, _, _ := server.snapshotState(); idx != wantIdx {
			t.Fatalf("target %d: server idx after pair = %d, want %d", i, idx, wantIdx)
		}
	}

	// Every target probed exactly once by each end.
	for i := 0; i < n; i++ {
		laneID := uint8(i + 1)
		if localProbesClient[laneID] != 1 {
			t.Errorf("target %d: client local probes = %d, want 1", i, localProbesClient[laneID])
		}
		if localProbesServer[laneID] != 1 {
			t.Errorf("target %d: server local probes = %d, want 1", i, localProbesServer[laneID])
		}
	}
}

// TestGateAdvanceIgnoresStaleKey verifies an advance for a non-current target is
// a no-op (gear-mesh safety: a late/duplicate signal must not skip the gate).
func TestGateAdvanceIgnoresStaleKey(t *testing.T) {
	targets := gateTestTargets(2)
	client := newGateOnly(true, targets)

	staleKey := LegKey{SessionID: 1, LaneID: 99, Kind: transport.KindUDP, Endpoint: "ep"}
	client.advanceAfterLocal(staleKey)
	if idx, ph, _ := client.snapshotState(); idx != 0 || ph != bwPhaseLocal {
		t.Fatalf("stale advance changed gate to (%d,%d), want (0,Local)", idx, ph)
	}

	// Wrong-phase advance is also a no-op: client starts Local, a Remote advance
	// for the current key must not move it.
	curKey := targets[0].key
	client.advanceAfterRemote(curKey)
	if idx, ph, _ := client.snapshotState(); idx != 0 || ph != bwPhaseLocal {
		t.Fatalf("wrong-phase advance changed gate to (%d,%d), want (0,Local)", idx, ph)
	}
}

// TestGateCompletesAfterAllTargets verifies the gate marks itself done once it
// steps past the last target.
func TestGateCompletesAfterAllTargets(t *testing.T) {
	targets := gateTestTargets(1)
	client := newGateOnly(true, targets)
	key := targets[0].key

	client.advanceAfterLocal(key) // Local→Remote(0)
	client.drainWake()
	client.advanceAfterRemote(key) // Remote→Local, idx=1 (past end)
	client.drainWake()

	if idx, _, _ := client.snapshotState(); idx != 1 {
		t.Fatalf("idx after last target = %d, want 1", idx)
	}
	// idx >= len(targets): the eval loop would set done; assert the index is past
	// the end so eval terminates.
	if idx, _, _ := client.snapshotState(); idx < len(targets) {
		t.Fatalf("gate did not advance past last target: idx=%d len=%d", idx, len(targets))
	}
}

// TestBuildBwTargetsOrder verifies target ordering: per lane ascending, TCP then
// UDP, and UDP-only when capBps>0 (spec 7.5).
func TestBuildBwTargetsOrder(t *testing.T) {
	lanes := map[laneKey]*laneRuntime{}
	mk := func(id uint8) *laneRuntime {
		l := newLaneRuntime(id, 100)
		l.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"})
		l.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "t"})
		return l
	}
	lanes[laneKey{sessionID: 1, laneID: 2}] = mk(2)
	lanes[laneKey{sessionID: 1, laneID: 1}] = mk(1)

	// capBps==0: TCP then UDP per lane, lanes ascending.
	got := buildBwTargets(1, lanes, 0)
	want := []struct {
		lane uint8
		kind transport.Kind
	}{
		{1, transport.KindTCP}, {1, transport.KindUDP},
		{2, transport.KindTCP}, {2, transport.KindUDP},
	}
	if len(got) != len(want) {
		t.Fatalf("capBps=0 targets = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].laneID != w.lane || got[i].kind != w.kind {
			t.Errorf("target %d = (lane %d,kind %d), want (lane %d,kind %d)", i, got[i].laneID, got[i].kind, w.lane, w.kind)
		}
	}

	// capBps>0: UDP only.
	gotCap := buildBwTargets(1, lanes, 50_000_000)
	for _, target := range gotCap {
		if target.kind != transport.KindUDP {
			t.Errorf("capBps>0 should yield UDP-only, got kind %d", target.kind)
		}
	}
	if len(gotCap) != 2 {
		t.Fatalf("capBps>0 targets = %d, want 2 (UDP per lane)", len(gotCap))
	}
}

func TestGateInjectsCapAsUDPReference(t *testing.T) {
	const capBps = uint64(200_000_000)
	target := bwTarget{
		laneID: 1,
		kind:   transport.KindUDP,
		leg:    transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindUDP, Endpoint: "u"},
	}
	var got bwTarget
	s := newBwScheduler(true, capBps, 0,
		func() []bwTarget { return []bwTarget{target} },
		func(t bwTarget) *bw.BwLoop {
			got = t
			return nil
		},
		nil,
	)

	s.runLocal(context.Background(), target)
	if got.referenceBps != capBps || got.capBps != capBps {
		t.Fatalf("UDP target reference/cap = %d/%d, want %d/%d", got.referenceBps, got.capBps, capBps, capBps)
	}
}

func TestGateUsesExplicitReferenceBeforeCapOrTCPReference(t *testing.T) {
	const explicitReferenceBps = uint64(120_000_000)
	const capBps = uint64(200_000_000)
	target := bwTarget{
		laneID: 1,
		kind:   transport.KindUDP,
		leg:    transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindUDP, Endpoint: "u"},
	}
	var got bwTarget
	s := newBwScheduler(true, capBps, explicitReferenceBps,
		func() []bwTarget { return []bwTarget{target} },
		func(t bwTarget) *bw.BwLoop {
			got = t
			return nil
		},
		nil,
	)
	s.completeLocal(bwTarget{
		laneID: 1,
		kind:   transport.KindTCP,
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindTCP, Conn: "t"},
	}, bw.Sample{BandwidthBps: 300_000_000})

	s.runLocal(context.Background(), target)
	if got.referenceBps != explicitReferenceBps || got.capBps != capBps {
		t.Fatalf("explicit-ref UDP target reference/cap = %d/%d, want %d/%d", got.referenceBps, got.capBps, explicitReferenceBps, capBps)
	}

	tcpTarget := bwTarget{
		laneID: 1,
		kind:   transport.KindTCP,
		leg:    transport.LegRef{Kind: transport.KindTCP, ConnID: "t"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindTCP, Conn: "t"},
	}
	got = bwTarget{}
	s = newBwScheduler(true, 0, explicitReferenceBps,
		func() []bwTarget { return []bwTarget{tcpTarget, target} },
		func(t bwTarget) *bw.BwLoop {
			got = t
			return nil
		},
		nil,
	)
	s.targets = []bwTarget{tcpTarget, target}
	s.phase = bwPhaseLocal
	s.started = true
	s.completeLocal(tcpTarget, bw.Sample{BandwidthBps: 300_000_000})
	s.drainWake()
	s.advanceAfterRemote(tcpTarget.key)
	s.drainWake()

	s.runLocal(context.Background(), target)
	if got.referenceBps != explicitReferenceBps || got.capBps != 0 {
		t.Fatalf("explicit-ref UDP target after TCP reference = %d/%d, want %d/0", got.referenceBps, got.capBps, explicitReferenceBps)
	}
}

func TestGateInjectsTCPReferenceIntoUncappedUDP(t *testing.T) {
	const tcpReferenceBps = uint64(300_000_000)
	tcpTarget := bwTarget{
		laneID: 1,
		kind:   transport.KindTCP,
		leg:    transport.LegRef{Kind: transport.KindTCP, ConnID: "t"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindTCP, Conn: "t"},
	}
	udpTarget := bwTarget{
		laneID: 1,
		kind:   transport.KindUDP,
		leg:    transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindUDP, Endpoint: "u"},
	}
	var got bwTarget
	s := newBwScheduler(true, 0, 0,
		func() []bwTarget { return []bwTarget{tcpTarget, udpTarget} },
		func(t bwTarget) *bw.BwLoop {
			got = t
			return nil
		},
		nil,
	)
	s.targets = []bwTarget{tcpTarget, udpTarget}
	s.phase = bwPhaseLocal
	s.started = true

	s.completeLocal(tcpTarget, bw.Sample{BandwidthBps: tcpReferenceBps})
	s.drainWake()
	s.advanceAfterRemote(tcpTarget.key)
	s.drainWake()
	s.runLocal(context.Background(), udpTarget)

	if got.referenceBps != tcpReferenceBps || got.capBps != tcpReferenceBps {
		t.Fatalf("uncapped UDP target reference/cap = %d/%d, want TCP reference %d/%d", got.referenceBps, got.capBps, tcpReferenceBps, tcpReferenceBps)
	}
}

func TestGateIgnoresStaleTCPReferenceSample(t *testing.T) {
	staleTCP := bwTarget{
		laneID: 1,
		kind:   transport.KindTCP,
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindTCP, Conn: "stale"},
	}
	currentTCP := bwTarget{
		laneID: 2,
		kind:   transport.KindTCP,
		key:    LegKey{SessionID: 1, LaneID: 2, Kind: transport.KindTCP, Conn: "current"},
	}
	udpTarget := bwTarget{
		laneID: 1,
		kind:   transport.KindUDP,
		leg:    transport.LegRef{Kind: transport.KindUDP, EndpointID: "u"},
		key:    LegKey{SessionID: 1, LaneID: 1, Kind: transport.KindUDP, Endpoint: "u"},
	}
	var got bwTarget
	s := newBwScheduler(true, 0, 0,
		func() []bwTarget { return []bwTarget{currentTCP} },
		func(t bwTarget) *bw.BwLoop {
			got = t
			return nil
		},
		nil,
	)
	s.targets = []bwTarget{currentTCP}
	s.phase = bwPhaseLocal
	s.started = true

	s.completeLocal(staleTCP, bw.Sample{BandwidthBps: 300_000_000})
	s.runLocal(context.Background(), udpTarget)

	if got.referenceBps != 0 || got.capBps != 0 {
		t.Fatalf("stale TCP sample leaked into UDP reference/cap = %d/%d, want 0/0", got.referenceBps, got.capBps)
	}
}

// TestGateAbortReleasesGate verifies that aborting the active local probe (e.g.
// the leg died mid-train) releases the gate by advancing past the current phase
// (spec 7.5: leg 死 → 中止 train + 释放 gate). The scheduler holds no lane
// reference in the gate path, so abort can never mark a lane down.
func TestGateAbortReleasesGate(t *testing.T) {
	targets := gateTestTargets(2)
	client := newGateOnly(true, targets) // client starts Local on target 0
	key := targets[0].key

	// current is nil (loop already ended on leg death); abort must still advance.
	client.abort(key)
	client.drainWake()

	// Client Local→Remote on the same target (gate released, ready for peer).
	if idx, ph, _ := client.snapshotState(); idx != 0 || ph != bwPhaseRemote {
		t.Fatalf("after abort gate = (%d,%d), want (0,Remote)", idx, ph)
	}
}

// TestGateRemoteTimeoutAdvances verifies the SRTT fallback: when this end waits
// in the Remote phase and the peer never probes, the gate self-advances after
// the timeout so the sweep does not stall (spec 7.5: 超时兜底 + 齿轮咬合 self-heal).
func TestGateRemoteTimeoutAdvances(t *testing.T) {
	targets := gateTestTargets(1)

	// Server starts in Remote phase on target 0. newLoop returns nil (no real
	// probing); timeout is tiny so the remote fallback fires quickly. After the
	// fallback advances Remote→Local, runLocal sees a nil loop and self-advances
	// past the last target → done.
	s := newBwScheduler(false, 0, 0,
		func() []bwTarget { return targets },
		func(bwTarget) *bw.BwLoop { return nil },
		func(bwTarget) time.Duration { return 10 * time.Millisecond },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Swept to completion via the timeout fallback.
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("gate did not advance past the remote-wait timeout")
	}

	if _, _, isDone := s.snapshotState(); !isDone {
		t.Fatal("scheduler should be done after sweeping the single target")
	}
}
