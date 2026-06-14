package send

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/ping"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/recv"
)

type e2eAddr struct{ addr string }

func (a *e2eAddr) Network() string { return "udp" }
func (a *e2eAddr) String() string  { return a.addr }

func e2eUDP() transport.LegRef {
	return transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &e2eAddr{addr: "127.0.0.1:9000"}}
}

func e2eTCP(connID string) transport.LegRef {
	return transport.LegRef{Kind: transport.KindTCP, ConnID: connID}
}

type fakeStreamTransport struct {
	mu      sync.Mutex
	dials   []string
	closed  []string
	nextRef transport.LegRef
}

func (f *fakeStreamTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeStreamTransport) Dial(ctx context.Context, remote string) (transport.LegRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = append(f.dials, remote)
	ref := f.nextRef
	if ref.Kind == 0 {
		ref = e2eTCP(remote)
	}
	return ref, nil
}

func (f *fakeStreamTransport) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	return len(payload), nil
}

func (f *fakeStreamTransport) Close(ctx context.Context, connID string) error {
	f.mu.Lock()
	f.closed = append(f.closed, connID)
	f.mu.Unlock()
	return nil
}

func (f *fakeStreamTransport) closedConn(connID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, closed := range f.closed {
		if closed == connID {
			return true
		}
	}
	return false
}

type blockingCloseStreamTransport struct {
	closeStarted chan struct{}
	releaseClose chan struct{}
}

func (f *blockingCloseStreamTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *blockingCloseStreamTransport) Dial(ctx context.Context, remote string) (transport.LegRef, error) {
	return e2eTCP(remote), nil
}

func (f *blockingCloseStreamTransport) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	return len(payload), nil
}

func (f *blockingCloseStreamTransport) Close(ctx context.Context, connID string) error {
	close(f.closeStarted)
	select {
	case <-f.releaseClose:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// makeIPv4 builds a minimal valid IPv4 packet of totalLen bytes; byte 20 encodes
// id so recovered packets are identifiable.
func makeIPv4(id byte, totalLen int) []byte {
	if totalLen < 21 {
		totalLen = 21
	}
	p := make([]byte, totalLen)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(totalLen))
	p[20] = id
	return p
}

func drainRecv(r *recv.Recv) int {
	n := 0
	for {
		select {
		case p := <-r.Packets():
			p.Release()
			n++
		default:
			return n
		}
	}
}

func drainSendFrames(t *testing.T, s *Send) []protocol.Frame {
	t.Helper()
	var frames []protocol.Frame
	for {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil {
				frames = append(frames, f)
			}
		default:
			return frames
		}
	}
}

func TestBootstrapHelloAdvertisesFECOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		name        string
		enableFEC   bool
		wantCaps    uint16
		wantProfile uint8
	}{
		{
			name:        "disabled",
			enableFEC:   false,
			wantCaps:    0,
			wantProfile: protocol.FECProfileOff,
		},
		{
			name:        "enabled",
			enableFEC:   true,
			wantCaps:    protocol.CapFEC | protocol.CapLinkStatus,
			wantProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := &sessionpkg.Manager{}
			s := New(Config{
				SessionManager: sessions,
				BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
				ProbeInterval:  time.Hour,
				ProbeTimeout:   time.Hour,
			})
			if tt.enableFEC {
				s.EnableFEC()
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := s.Bootstrap(ctx); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			sessionID, ok := s.activeSession()
			if !ok {
				t.Fatal("no active session after bootstrap")
			}

			hello := waitForHello(t, s, sessionID, 1)
			if hello.Caps != tt.wantCaps {
				t.Fatalf("HELLO caps = %#x, want %#x", hello.Caps, tt.wantCaps)
			}
			if hello.FECProfile != tt.wantProfile {
				t.Fatalf("HELLO FEC profile = %d, want %d", hello.FECProfile, tt.wantProfile)
			}
		})
	}
}

func TestBootstrapHelloAdvertisesLinkStatusOnlyWithFEC(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})
	s.EnableFEC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	_ = drainSendFrames(t, s)
	sessionID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session")
	}
	hello := waitForHello(t, s, sessionID, 1)
	want := protocol.CapFEC | protocol.CapLinkStatus
	if hello.Caps != want {
		t.Fatalf("HELLO caps = %#x, want %#x", hello.Caps, want)
	}
}

// TestSendToRecvFECRecovery is the end-to-end data path: Send.Write produces a
// FEC'd group (4 DATA + 1 REPAIR); we drop one DATA and feed the rest into Recv,
// which must reconstruct the missing packet and dedupe a late duplicate.
//
// The lane runs single-transport (UDP only), so DATA and REPAIR share the UDP
// link — a valid degraded path. FEC recovery is independent of which leg
// carries the frames. It uses the public Bootstrap path to stand up the
// active session + lane.
func TestSendToRecvFECRecovery(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
	})
	s.EnableFEC()

	ctx := context.Background()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Bootstrap binds the UDP transport but does NOT mark it active: under plan
	// 6.2, active flips only on peer reply (PONG, via the ping OnUp closure).
	// This FEC recovery test focuses on the data path, not the handshake, so we
	// activate the lane directly to simulate a path that has already come up.
	// The real PONG→OnUp→markActive activation path is covered by
	// TestLaneActivatesViaPongThroughLaneManager below.
	s.lanesMu.RLock()
	for _, lane := range s.lanes {
		lane.markActive(transport.KindUDP)
	}
	s.lanesMu.RUnlock()
	s.EnableFEC()

	// Drain the bootstrap HELLO frame.
	for {
		select {
		case p := <-s.Packets():
			p.Packet.Release()
			continue
		default:
		}
		break
	}

	const group = 4
	for i := 0; i < group; i++ {
		pkt := packetbuf.Acquire(40)
		pkt.Payload = makeIPv4(byte(i+1), 40)
		if err := s.Write(ctx, pkt); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	var dataFrames []protocol.Frame
	var repairFrame *protocol.Frame
	for {
		select {
		case payload := <-s.Packets():
			f, derr := protocol.Decode(payload.Packet.Payload)
			if derr == nil {
				// Deep-copy the body slices before releasing the packet: Decode
				// aliases the packet buffer, which the pool reuses after Release.
				switch b := f.Body.(type) {
				case protocol.DataBody:
					b.Packet = append([]byte(nil), b.Packet...)
					f.Body = b
					dataFrames = append(dataFrames, f)
				case protocol.RepairBody:
					b.Symbol = append([]byte(nil), b.Symbol...)
					f.Body = b
					cp := f
					repairFrame = &cp
				}
			}
			payload.Packet.Release()
			continue
		default:
		}
		break
	}

	if len(dataFrames) != group {
		t.Fatalf("got %d DATA frames, want %d", len(dataFrames), group)
	}
	if repairFrame == nil {
		t.Fatal("no REPAIR frame produced")
	}

	r := recv.New(recv.Config{SessionManager: sessions})
	encode := func(f protocol.Frame) *packetbuf.Packet {
		b, e := protocol.Encode(f, nil)
		if e != nil {
			t.Fatalf("encode: %v", e)
		}
		p := packetbuf.Acquire(len(b))
		copy(p.Payload, b)
		return p
	}

	// Deliver DATA 0,1,2 (drop DATA 3), then REPAIR.
	for i := 0; i < group-1; i++ {
		if err := r.WriteTo(ctx, e2eUDP(), encode(dataFrames[i])); err != nil {
			t.Fatalf("recv DATA %d: %v", i, err)
		}
		drainRecv(r)
	}
	if err := r.WriteTo(ctx, e2eUDP(), encode(*repairFrame)); err != nil {
		t.Fatalf("recv REPAIR: %v", err)
	}

	if recovered := drainRecv(r); recovered == 0 {
		t.Fatal("expected FEC to recover the dropped DATA packet")
	}

	// Late duplicate of DATA 3 must be deduped.
	if err := r.WriteTo(ctx, e2eUDP(), encode(dataFrames[group-1])); err != nil {
		t.Fatalf("recv late DATA: %v", err)
	}
	if extra := drainRecv(r); extra != 0 {
		t.Errorf("late duplicate produced %d TUN packets, want 0", extra)
	}
}

// TestLaneActivatesViaPongThroughLaneManager verifies the stage③ liveness path
// end to end: Bootstrap registers a dead ping per lane in the shared LaneManager
// (startLanePing). A lane starts NOT ready (active flips only on peer reply,
// spec 6.2). Feeding RecoverSuccess PONGs through LaneManager.LookupPing(...).Pong
// crosses the recovery threshold, fires the ping's OnUp closure (leg.markActive),
// and the lane becomes ready — all without the recv side touching lane/leg state.
func TestLaneActivatesViaPongThroughLaneManager(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  20 * time.Millisecond,
		ProbeTimeout:   time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sessionID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}
	lane := s.getLane(laneKey{sessionID: sessionID, laneID: 1})
	if lane == nil {
		t.Fatal("bootstrap lane not found")
	}

	// A freshly bootstrapped lane is bound but not active: not ready yet.
	if lane.ready() {
		t.Fatal("lane should NOT be ready before any PONG (spec 6.2)")
	}

	// The send side registered a dead ping for this leg in the LaneManager.
	key := KeyForLeg(sessionID, 1, e2eUDP())
	p := s.LaneManager().LookupPing(key)
	if p == nil {
		t.Fatal("expected a ping registered in LaneManager for the bootstrap leg")
	}

	// Feed PONGs the way the recv glue does: read each emitted PING and echo a
	// matching PONG into the same ping via Pong. Default RecoverSuccess is 3, so
	// at least 3 valid pongs bring the dead ping (and thus the leg) up.
	deadline := time.Now().Add(2 * time.Second)
	for !lane.ready() && time.Now().Before(deadline) {
		msg := waitForPing(t, s, sessionID, 1)
		p.Pong(msg, msg.TimeMS+5)
	}

	if !lane.ready() {
		t.Fatal("lane did not become ready after feeding recovery PONGs through LaneManager")
	}
}

func TestBootstrapLaneActivatesOnAcceptedHelloAck(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sessionID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}
	lane := s.getLane(laneKey{sessionID: sessionID, laneID: 1})
	if lane == nil {
		t.Fatal("bootstrap lane not found")
	}
	if lane.ready() {
		t.Fatal("lane should not be ready before HELLO_ACK")
	}

	hello := waitForHello(t, s, sessionID, 1)
	if _, ok := sessions.Get(sessionID); !ok {
		t.Fatal("bootstrap session missing")
	}
	if !sessionsAckHello(t, sessions, sessionID, hello.Nonce) {
		t.Fatal("session Ack rejected accepted HELLO_ACK")
	}
	if !lane.ready() {
		t.Fatal("accepted HELLO_ACK did not mark bootstrap leg active")
	}
}

func TestTCPFallbackRequiresHelloAckBeforeActive(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	stream := &fakeStreamTransport{nextRef: e2eTCP("tcp-1")}
	s := New(Config{
		SessionManager:  sessions,
		StreamTransport: stream,
		BootstrapLanes: []BootstrapLane{{
			LaneID:    1,
			Weight:    100,
			Leg:       e2eUDP(),
			TCPRemote: "tcp-remote",
		}},
		ProbeInterval: time.Hour,
		ProbeTimeout:  time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sessionID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}
	lane := s.getLane(laneKey{sessionID: sessionID, laneID: 1})
	if lane == nil {
		t.Fatal("bootstrap lane not found")
	}

	hello := waitForHelloOnLeg(t, s, sessionID, 1, transport.KindTCP)
	if lane.leg.isActive(transport.KindTCP) {
		t.Fatal("TCP fallback became active before TCP HELLO_ACK")
	}
	if !sessionsAckHello(t, sessions, sessionID, hello.Nonce) {
		t.Fatal("session Ack rejected TCP HELLO_ACK")
	}
	if !lane.leg.isActive(transport.KindTCP) {
		t.Fatal("TCP fallback did not become active after TCP HELLO_ACK")
	}
}

func TestUncappedBandwidthSchedulerWaitsForTCPHelloAck(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	stream := &fakeStreamTransport{nextRef: e2eTCP("tcp-bw")}
	s := New(Config{
		SessionManager:       sessions,
		StreamTransport:      stream,
		EnableBandwidthProbe: true,
		IsClient:             true,
		BootstrapLanes: []BootstrapLane{{
			LaneID:    1,
			Weight:    100,
			Leg:       e2eUDP(),
			TCPRemote: "tcp-remote",
		}},
		ProbeInterval: time.Hour,
		ProbeTimeout:  time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	sessionID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}

	hello := waitForHelloOnLeg(t, s, sessionID, 1, transport.KindTCP)
	if s.getBwScheduler() != nil {
		t.Fatal("uncapped bandwidth scheduler started before TCP HELLO_ACK")
	}
	if !sessionsAckHello(t, sessions, sessionID, hello.Nonce) {
		t.Fatal("session Ack rejected TCP HELLO_ACK")
	}

	sched := waitForBwScheduler(t, s)
	targets := waitForBwTargets(t, sched)
	if len(targets) < 2 {
		t.Fatalf("bandwidth scheduler targets = %d, want TCP and UDP", len(targets))
	}
	if targets[0].kind != transport.KindTCP {
		t.Fatalf("first bandwidth target kind = %d, want TCP", targets[0].kind)
	}
}

func waitForBwScheduler(t *testing.T, s *Send) *bwScheduler {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sched := s.getBwScheduler(); sched != nil {
			return sched
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for bandwidth scheduler")
	return nil
}

func waitForBwTargets(t *testing.T, sched *bwScheduler) []bwTarget {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sched.mu.Lock()
		targets := append([]bwTarget(nil), sched.targets...)
		started := sched.started
		sched.mu.Unlock()
		if started && len(targets) > 0 {
			return targets
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for bandwidth scheduler targets")
	return nil
}

func TestOnLegFailureOnlyMarksMatchingTCPConnDown(t *testing.T) {
	s := New()
	const sessionID = uint64(91)
	s.activateSession(sessionID)

	lane1 := newLaneRuntime(1, 100)
	lane1.bindTCP(e2eTCP("tcp-1"))
	lane1.markActive(transport.KindTCP)
	lane1.dialer = newDialer("remote-1", nil, nil)

	lane2 := newLaneRuntime(2, 100)
	lane2.bindTCP(e2eTCP("tcp-2"))
	lane2.markActive(transport.KindTCP)
	lane2.dialer = newDialer("remote-2", nil, nil)

	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane1
	s.lanes[laneKey{sessionID: sessionID, laneID: 2}] = lane2
	s.lanesMu.Unlock()

	s.OnLegFailure(context.Background(), e2eTCP("tcp-1"), context.Canceled)

	if lane1.leg.isActive(transport.KindTCP) {
		t.Fatal("failed TCP conn stayed active")
	}
	if !lane2.leg.isActive(transport.KindTCP) {
		t.Fatal("unrelated TCP conn was marked down")
	}
}

// waitForPing blocks until a PING frame for the given session/lane is emitted by
// the send side, returning its ping.Message fields. It fails the test on timeout.
func waitForPing(t *testing.T, s *Send, sessionID uint64, laneID uint8) ping.Message {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err != nil {
				continue
			}
			if f.Type == protocol.TypePING && f.SessionID == sessionID && f.LaneID == laneID {
				b := f.Body.(protocol.PingBody)
				return ping.Message{ID: b.PingID, TimeMS: b.TimeMS}
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out waiting for a PING frame")
	return ping.Message{}
}

func waitForHello(t *testing.T, s *Send, sessionID uint64, laneID uint8) protocol.HelloBody {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err != nil {
				continue
			}
			if f.Type == protocol.TypeHELLO && f.SessionID == sessionID && f.LaneID == laneID {
				return f.Body.(protocol.HelloBody)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out waiting for a HELLO frame")
	return protocol.HelloBody{}
}

func waitForHelloOnLeg(t *testing.T, s *Send, sessionID uint64, laneID uint8, kind transport.Kind) protocol.HelloBody {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			legKind := payload.Leg.Kind
			payload.Packet.Release()
			if err != nil {
				continue
			}
			if f.Type == protocol.TypeHELLO && f.SessionID == sessionID && f.LaneID == laneID && legKind == kind {
				return f.Body.(protocol.HelloBody)
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("timed out waiting for a HELLO frame on leg kind %d", kind)
	return protocol.HelloBody{}
}

func sessionsAckHello(t *testing.T, sessions *sessionpkg.Manager, sessionID uint64, nonce uint64) bool {
	t.Helper()
	sess, ok := sessions.Get(sessionID)
	if !ok {
		return false
	}
	return sess.Ack(nonce, true)
}

// TestLaneGoesDownViaPingTimeout verifies the OnDown→markDown wiring end to end:
// once a lane is up, ceasing to answer its pings lets consecutive timeouts cross
// MaxLoss, firing the ping's OnDown closure (leg.markDown) so the lane stops being
// ready. The recv side is not involved — death is driven entirely by the send-side
// ping's own timeout accounting (spec 6.2: UDP 靠 ping 连续超时判死).
func TestLaneGoesDownViaPingTimeout(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  15 * time.Millisecond,
		ProbeTimeout:   20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	sessionID, _ := s.activeSession()
	lane := s.getLane(laneKey{sessionID: sessionID, laneID: 1})
	key := KeyForLeg(sessionID, 1, e2eUDP())
	p := s.LaneManager().LookupPing(key)
	if p == nil {
		t.Fatal("expected a registered ping")
	}

	// Bring the lane up by answering a few pings.
	deadline := time.Now().Add(2 * time.Second)
	for !lane.ready() && time.Now().Before(deadline) {
		msg := waitForPing(t, s, sessionID, 1)
		p.Pong(msg, msg.TimeMS+2)
	}
	if !lane.ready() {
		t.Fatal("lane never came up")
	}

	// Stop answering. The ping's Start loop accumulates timeouts; after MaxLoss
	// (default 3) consecutive losses it fires OnDown → leg.markDown → not ready.
	downDeadline := time.Now().Add(2 * time.Second)
	for lane.ready() && time.Now().Before(downDeadline) {
		// Drain (and discard) emitted PINGs without answering, so each one times out.
		select {
		case payload := <-s.Packets():
			payload.Packet.Release()
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if lane.ready() {
		t.Fatal("lane did not go down after MaxLoss consecutive ping timeouts")
	}
}

// TestRebootstrapBuildsFreshSession verifies the unknown-session self-heal on the
// client side: after a Rebootstrap, the active session id changes, the old
// session is removed from the manager, and a fresh lane + ping is registered.
// The old session's goroutines are cancelled via its per-session context.
func TestRebootstrapBuildsFreshSession(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  time.Hour, // keep pings quiet; we only check identity
		ProbeTimeout:   time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	oldID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}
	if _, known := sessions.Get(oldID); !known {
		t.Fatal("bootstrap session not in manager")
	}

	if err := s.Rebootstrap(); err != nil {
		t.Fatalf("rebootstrap: %v", err)
	}

	newID, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after rebootstrap")
	}
	if newID == oldID {
		t.Fatalf("rebootstrap reused session id %d, want a fresh one", oldID)
	}
	if _, known := sessions.Get(oldID); known {
		t.Error("old session still in manager after rebootstrap")
	}
	if _, known := sessions.Get(newID); !known {
		t.Error("new session missing from manager after rebootstrap")
	}

	// A fresh lane + ping must exist for the new session.
	lane := s.getLane(laneKey{sessionID: newID, laneID: 1})
	if lane == nil {
		t.Fatal("no lane for the rebuilt session")
	}
	if p := s.LaneManager().LookupPing(KeyForLeg(newID, 1, e2eUDP())); p == nil {
		t.Error("no ping registered for the rebuilt session")
	}
	// The old session's ping must be gone (LaneManager was reset).
	if p := s.LaneManager().LookupPing(KeyForLeg(oldID, 1, e2eUDP())); p != nil {
		t.Error("stale ping for the old session survived rebootstrap")
	}
}

func TestRebootstrapSerializesConcurrentCalls(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	const workers = 8
	start := make(chan struct{})
	done := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			done <- s.Rebootstrap()
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-done; err != nil {
			t.Fatalf("rebootstrap failed: %v", err)
		}
	}

	active, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after concurrent rebootstrap")
	}
	seenActive := false
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && frame.Type == protocol.TypeHELLO {
				if frame.SessionID == active {
					seenActive = true
					continue
				}
				if _, known := sessions.Get(frame.SessionID); known {
					t.Fatalf("stale rebootstrap session %d remains known; active=%d", frame.SessionID, active)
				}
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !seenActive {
		t.Fatal("did not observe HELLO for active session")
	}
}

// TestRebootstrapServerSideNoOp verifies that an end with no bootstrap lanes
// (server) does not rebuild on Rebootstrap — it awaits the peer's fresh HELLO.
func TestRebootstrapServerSideNoOp(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{SessionManager: sessions}) // no bootstrap lanes

	if err := s.Rebootstrap(); err != nil {
		t.Fatalf("server-side Rebootstrap should be a no-op, got: %v", err)
	}
	if _, ok := s.activeSession(); ok {
		t.Error("server-side Rebootstrap should not activate a session")
	}
}

// TestAcceptedHelloAckPassivelyAdmitsServerLane verifies the server-side passive
// HELLO path: an end with no BootstrapLanes accepts a peer HELLO by writing an
// accepted HELLO_ACK on the observed leg. That outbound ACK is the admission
// point for creating this end's send-side lane, so later TUN packets can be sent
// back through Send.Write.
func TestAcceptedHelloAckPassivelyAdmitsServerLane(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	const sessionID = uint64(77)
	const laneID = uint8(4)
	if _, ok := sessions.GetOrCreate(sessionID); !ok {
		t.Fatal("failed to create passive session")
	}

	leg := e2eUDP()
	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    laneID,
		Body: protocol.HelloAckBody{
			Nonce:      12,
			Accepted:   1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}
	if err := s.WriteFrame(context.Background(), ack, leg); err != nil {
		t.Fatalf("WriteFrame HELLO_ACK: %v", err)
	}

	if cur, ok := s.activeSession(); !ok || cur != sessionID {
		t.Fatalf("passive HELLO_ACK did not activate session: ok=%v cur=%d want %d", ok, cur, sessionID)
	}
	lane := s.getLane(laneKey{sessionID: sessionID, laneID: laneID})
	if lane == nil {
		t.Fatal("passive HELLO_ACK did not create lane")
	}
	if !lane.ready() {
		t.Fatal("passive HELLO_ACK did not mark observed leg active")
	}

	pkt := packetbuf.Acquire(40)
	pkt.Payload = makeIPv4(9, 40)
	if err := s.Write(context.Background(), pkt); err != nil {
		t.Fatalf("Write after passive admission: %v", err)
	}

	frames := drainSendFrames(t, s)
	var sawData bool
	for _, f := range frames {
		if f.Type == protocol.TypeDATA && f.SessionID == sessionID && f.LaneID == laneID {
			sawData = true
		}
	}
	if !sawData {
		t.Fatal("expected DATA after passive admission")
	}
}

func TestAcceptedTCPHelloAckDoesNotStartPing(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	const sessionID = uint64(78)
	const laneID = uint8(5)
	if _, ok := sessions.GetOrCreate(sessionID); !ok {
		t.Fatal("failed to create passive session")
	}

	leg := e2eTCP("tcp-passive")
	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    laneID,
		Body: protocol.HelloAckBody{
			Nonce:    12,
			Accepted: 1,
		},
	}
	if err := s.WriteFrame(context.Background(), ack, leg); err != nil {
		t.Fatalf("WriteFrame HELLO_ACK: %v", err)
	}

	lane := s.getLane(laneKey{sessionID: sessionID, laneID: laneID})
	if lane == nil {
		t.Fatal("passive TCP HELLO_ACK did not create lane")
	}
	if !lane.leg.isActive(transport.KindTCP) {
		t.Fatal("passive TCP HELLO_ACK did not mark TCP active")
	}
	if p := s.laneManager.LookupPing(KeyForLeg(sessionID, laneID, leg)); p != nil {
		t.Fatal("passive TCP HELLO_ACK registered a ping loop")
	}
}

func TestStartLanePingIsIdempotentForSameLeg(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	const sessionID = uint64(91)
	const laneID = uint8(2)
	if _, ok := sessions.GetOrCreate(sessionID); !ok {
		t.Fatal("failed to create session")
	}
	lane := newLaneRuntime(laneID, 1)
	leg := e2eUDP()
	lane.bindUDP(leg)
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: laneID}] = lane
	s.lanesMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 8; i++ {
		s.startLanePing(ctx, sessionID, lane, leg)
	}

	deadline := time.After(500 * time.Millisecond)
	pingFrames := 0
	for pingFrames == 0 {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err != nil {
				t.Fatalf("decode sent frame: %v", err)
			}
			if frame.Type == protocol.TypePING {
				pingFrames++
			}
		case <-deadline:
			t.Fatal("timed out waiting for initial ping")
		}
	}

	quiet := time.NewTimer(50 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err != nil {
				t.Fatalf("decode sent frame: %v", err)
			}
			if frame.Type == protocol.TypePING {
				pingFrames++
			}
		case <-quiet.C:
			if pingFrames != 1 {
				t.Fatalf("duplicate ping loops started: got %d initial PING frames, want 1", pingFrames)
			}
			return
		}
	}
}

func TestStartBwSchedulerIsIdempotent(t *testing.T) {
	s := New(Config{
		EnableBandwidthProbe: true,
	})

	const sessionID = uint64(92)
	const laneID = uint8(3)
	lane := newLaneRuntime(laneID, 1)
	lane.bindUDP(e2eUDP())
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: laneID}] = lane
	s.lanesMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.startBwScheduler(ctx, sessionID)
	if s.bwSched == nil {
		t.Fatal("bandwidth scheduler was not started")
	}
	first := s.bwSched

	for i := 0; i < 8; i++ {
		s.startBwScheduler(ctx, sessionID)
	}
	if s.bwSched != first {
		t.Fatal("duplicate bandwidth scheduler start replaced the active scheduler")
	}
}

func TestPassiveHelloAckWaitsForRemoteBandwidthProbe(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager:       sessions,
		EnableBandwidthProbe: true,
	})

	const sessionID = uint64(940)
	if _, ok := sessions.GetOrCreate(sessionID); !ok {
		t.Fatal("failed to create session")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: sessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:    1,
			Accepted: 1,
		},
	}
	tcp := e2eTCP("passive-tcp")
	if err := s.WriteFrame(ctx, ack, tcp); err != nil {
		t.Fatalf("WriteFrame TCP HELLO_ACK: %v", err)
	}
	if s.bwSched != nil {
		t.Fatal("passive HELLO_ACK started bandwidth scheduler")
	}

	s.LaneManager().RemoteProbe(KeyForLeg(sessionID, 1, tcp))
	if s.bwSched == nil {
		t.Fatal("remote BW_PROBE did not start passive bandwidth scheduler")
	}
}

func TestConcurrentPassiveAdmissionsLeaveOnlyActiveSessionState(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const calls = 64
	var wg sync.WaitGroup
	wg.Add(calls)
	for i := 0; i < calls; i++ {
		sessionID := uint64(1000 + i)
		if _, ok := sessions.GetOrCreate(sessionID); !ok {
			t.Fatalf("failed to create session %d", sessionID)
		}
		go func(sessionID uint64) {
			defer wg.Done()
			ack := protocol.Frame{
				Version:   protocol.Version,
				Type:      protocol.TypeHELLOACK,
				SessionID: sessionID,
				LaneID:    1,
				Body: protocol.HelloAckBody{
					Nonce:    1,
					Accepted: 1,
				},
			}
			if err := s.WriteFrame(ctx, ack, e2eUDP()); err != nil {
				t.Errorf("WriteFrame HELLO_ACK session=%d: %v", sessionID, err)
			}
		}(sessionID)
	}
	wg.Wait()

	active, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after passive admissions")
	}
	s.lanesMu.RLock()
	laneSessions := make(map[uint64]struct{})
	for key := range s.lanes {
		laneSessions[key.sessionID] = struct{}{}
	}
	s.lanesMu.RUnlock()
	if len(laneSessions) != 1 {
		t.Fatalf("passive admissions left %d session lane sets, want 1: %#v", len(laneSessions), laneSessions)
	}
	if _, ok := laneSessions[active]; !ok {
		t.Fatalf("only lane session does not match active session %d: %#v", active, laneSessions)
	}

	s.sendStatesMu.RLock()
	sendStates := make(map[uint64]struct{})
	for sessionID := range s.sendStates {
		sendStates[sessionID] = struct{}{}
	}
	s.sendStatesMu.RUnlock()
	if len(sendStates) != 1 {
		t.Fatalf("passive admissions left %d send states, want 1: %#v", len(sendStates), sendStates)
	}
	if _, ok := sendStates[active]; !ok {
		t.Fatalf("only send state does not match active session %d: %#v", active, sendStates)
	}
}

func TestBandwidthSchedulerTeardownCanRaceWithRemoteComplete(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager:       sessions,
		EnableBandwidthProbe: true,
	})

	const sessionID = uint64(93)
	const laneID = uint8(1)
	if _, ok := sessions.GetOrCreate(sessionID); !ok {
		t.Fatal("failed to create session")
	}
	s.activateSession(sessionID)
	lane := newLaneRuntime(laneID, 1)
	leg := e2eUDP()
	lane.bindUDP(leg)
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: sessionID, laneID: laneID}] = lane
	s.lanesMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startBwScheduler(ctx, sessionID)
	if s.bwSched == nil {
		t.Fatal("bandwidth scheduler was not started")
	}

	key := KeyForLeg(sessionID, laneID, leg)
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		<-start
		for i := 0; i < 1000; i++ {
			s.LaneManager().RemoteComplete(key)
		}
		done <- struct{}{}
	}()
	go func() {
		<-start
		for i := 0; i < 1000; i++ {
			s.teardownSession(sessionID)
			s.activateSession(sessionID)
			s.startBwScheduler(ctx, sessionID)
		}
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done
}

func TestCloseSessionDoesNotResetNewPassiveAdmission(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	stream := &blockingCloseStreamTransport{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
	s := New(Config{
		SessionManager:  sessions,
		StreamTransport: stream,
		ProbeInterval:   time.Hour,
		ProbeTimeout:    time.Hour,
	})

	const oldSessionID = uint64(94)
	const newSessionID = uint64(95)
	const laneID = uint8(1)
	if _, ok := sessions.GetOrCreate(oldSessionID); !ok {
		t.Fatal("failed to create old session")
	}
	if _, ok := sessions.GetOrCreate(newSessionID); !ok {
		t.Fatal("failed to create new session")
	}
	s.activateSession(oldSessionID)
	oldLane := newLaneRuntime(laneID, 1)
	oldLane.bindUDP(e2eUDP())
	oldLane.bindTCP(e2eTCP("old-tcp"))
	s.lanesMu.Lock()
	s.lanes[laneKey{sessionID: oldSessionID, laneID: laneID}] = oldLane
	s.lanesMu.Unlock()
	s.registerLaneQoS(oldSessionID, oldLane)
	s.startLanePing(context.Background(), oldSessionID, oldLane, e2eUDP())

	doneClose := make(chan struct{})
	go func() {
		s.CloseSession(oldSessionID)
		close(doneClose)
	}()
	<-stream.closeStarted

	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: newSessionID,
		LaneID:    laneID,
		Body: protocol.HelloAckBody{
			Nonce:    1,
			Accepted: 1,
		},
	}
	if err := s.WriteFrame(context.Background(), ack, e2eUDP()); err != nil {
		t.Fatalf("passive admission while close is blocked: %v", err)
	}

	close(stream.releaseClose)
	<-doneClose

	if active, ok := s.activeSession(); !ok || active != newSessionID {
		t.Fatalf("active session = %d/%v, want new session %d", active, ok, newSessionID)
	}
	if s.getLane(laneKey{sessionID: newSessionID, laneID: laneID}) == nil {
		t.Fatal("new passive lane was removed by old CloseSession")
	}
	if s.LaneManager().LookupPing(KeyForLeg(newSessionID, laneID, e2eUDP())) == nil {
		t.Fatal("new passive ping registration was reset by old CloseSession")
	}
	if s.LaneManager().LookupQoS(LaneKey{SessionID: newSessionID, LaneID: laneID}) == nil {
		t.Fatal("new passive QoS registration was reset by old CloseSession")
	}
}

// TestCloseSessionClearsState verifies a plain session CLOSE releases all
// send-side per-session state (lane, sendState, ping) so it does not leak under
// session churn (spec 7). Unlike Rebootstrap, it does not rebuild.
func TestCloseSessionClearsState(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	stream := &fakeStreamTransport{}
	s := New(Config{
		SessionManager:  sessions,
		StreamTransport: stream,
		BootstrapLanes:  []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:   time.Hour,
		ProbeTimeout:    time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	id, ok := s.activeSession()
	if !ok {
		t.Fatal("no active session after bootstrap")
	}
	// State present before close.
	lane := s.getLane(laneKey{sessionID: id, laneID: 1})
	if lane == nil {
		t.Fatal("lane missing before close")
	}
	lane.bindTCP(e2eTCP("tcp-close"))
	lane.markActive(transport.KindTCP)
	lane.commitPacket(1, []byte("pending-fec"))
	if s.LaneManager().LookupPing(KeyForLeg(id, 1, e2eUDP())) == nil {
		t.Fatal("ping missing before close")
	}

	s.CloseSession(id)

	// All per-session state must be gone, and no active session remains.
	if _, ok := s.activeSession(); ok {
		t.Error("session still active after CloseSession")
	}
	if s.getLane(laneKey{sessionID: id, laneID: 1}) != nil {
		t.Error("lane survived CloseSession")
	}
	if s.getSendState(id) != nil {
		t.Error("sendState survived CloseSession")
	}
	if s.LaneManager().LookupPing(KeyForLeg(id, 1, e2eUDP())) != nil {
		t.Error("ping survived CloseSession (LaneManager not reset)")
	}
	if _, known := sessions.Get(id); known {
		t.Error("session survived in manager after CloseSession")
	}
	if !stream.closedConn("tcp-close") {
		t.Error("TCP conn was not closed during CloseSession")
	}
	if len(lane.txWindow.pending) != 0 {
		t.Error("pending FEC packets survived CloseSession")
	}
}

// TestCloseSessionWrongIDNoOp verifies CloseSession for a session this end does
// not hold is a no-op (does not tear down the active session).
func TestCloseSessionWrongIDNoOp(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := New(Config{
		SessionManager: sessions,
		BootstrapLanes: []BootstrapLane{{LaneID: 1, Weight: 100, Leg: e2eUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	id, _ := s.activeSession()

	s.CloseSession(id + 99) // not the held session

	if cur, ok := s.activeSession(); !ok || cur != id {
		t.Fatalf("active session changed after wrong-id CloseSession: ok=%v cur=%d want %d", ok, cur, id)
	}
	if s.getLane(laneKey{sessionID: id, laneID: 1}) == nil {
		t.Error("lane wrongly torn down by wrong-id CloseSession")
	}
}
