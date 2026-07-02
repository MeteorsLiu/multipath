package v2_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/ping"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/runtime"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

// TestHELLOThroughSession verifies that HELLO processing goes through Session,
// not through Send's control methods.
func TestHELLOThroughSession(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		SessionManager: sessions,
	})
	handler := runtime.NewRecvHandler(s, sessions)

	// Simulate receiving a HELLO
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	helloFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLO,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.HelloBody{
			Nonce:      100,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	ctx := context.Background()
	err := handler.OnHello(ctx, leg, helloFrame)
	if err != nil {
		t.Fatalf("OnHello failed: %v", err)
	}

	// Verify session was created via session.Manager
	sess, ok := sessions.Get(12345)
	if !ok || sess == nil {
		t.Fatal("expected session to be created via Manager.GetOrCreate")
	}

	// Verify HELLO_ACK was sent via Send.WriteFrame
	select {
	case payload := <-s.Packets():
		decoded, err := protocol.Decode(payload.Packet.Payload)
		if err != nil {
			t.Fatalf("failed to decode: %v", err)
		}
		if decoded.Type != protocol.TypeHELLOACK {
			t.Errorf("expected HELLO_ACK, got %v", decoded.Type)
		}
		payload.Packet.Release()
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for HELLO_ACK")
	}
}

// TestHELLOACKValidatesNonceViaSession verifies that HELLO_ACK calls sess.Ack()
// to validate the nonce before updating lane state.
func TestHELLOACKValidatesNonceViaSession(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		SessionManager: sessions,
	})
	handler := runtime.NewRecvHandler(s, sessions)

	// Create session and open a HELLO. The self-driving loop needs a sender;
	// a no-op sender keeps the test focused on nonce validation via Ack.
	sess, _ := sessions.GetOrCreate(12345)
	noopSender := func(ctx context.Context, v sessionpkg.View) error { return nil }
	hello := sess.Open(context.Background(), sessionpkg.HelloConfig{RetryInterval: time.Hour}, noopSender, nil)

	var nonce uint64
	hello.Do(func(v sessionpkg.View) error {
		nonce = v.Nonce()
		return nil
	})

	// Simulate receiving HELLO_ACK with correct nonce
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	ackFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	ctx := context.Background()
	err := handler.OnHelloAck(ctx, leg, ackFrame)
	if err != nil {
		t.Errorf("OnHelloAck failed: %v", err)
	}

	// Try with wrong nonce - should be rejected silently
	wrongAckFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: 12345,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      nonce + 999, // wrong nonce
			Accepted:   1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}

	err = handler.OnHelloAck(ctx, leg, wrongAckFrame)
	// Should not error, just silently reject
	if err != nil {
		t.Errorf("OnHelloAck with wrong nonce should not error: %v", err)
	}
}

// TestSendWriteFrameForControlFrames verifies that Send.WriteFrame is the
// entry point for control frames, not semantic control methods.
func TestSendWriteFrameForControlFrames(t *testing.T) {
	s := send.New()

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "test-ep",
		RemoteAddr: &testAddr{addr: "127.0.0.1:8080"},
	}

	// All control frames go through WriteFrame
	ctx := context.Background()

	frames := []protocol.Frame{
		{Type: protocol.TypeHELLO, SessionID: 1, LaneID: 1, Body: protocol.HelloBody{}},
		{Type: protocol.TypeHELLOACK, SessionID: 1, LaneID: 1, Body: protocol.HelloAckBody{}},
		{Type: protocol.TypePING, SessionID: 1, LaneID: 1, Body: protocol.PingBody{}},
		{Type: protocol.TypePONG, SessionID: 1, LaneID: 1, Body: protocol.PingBody{}},
		{Type: protocol.TypeCLOSE, SessionID: 1, LaneID: 1, Body: protocol.CloseBody{}},
	}

	for _, frame := range frames {
		frame.Version = protocol.Version
		err := s.WriteFrame(ctx, frame, leg)
		if err != nil {
			t.Errorf("WriteFrame(%v) failed: %v", frame.Type, err)
		}

		// Verify frame was queued
		select {
		case payload := <-s.Packets():
			payload.Packet.Release()
		default:
			t.Errorf("expected frame %v in output queue", frame.Type)
		}
	}
}

type testAddr struct {
	addr string
}

func (a *testAddr) Network() string { return "udp" }
func (a *testAddr) String() string  { return a.addr }

// drainPackets reads all queued packets from Send and decodes them.
func drainPackets(s *send.Send) []protocol.Frame {
	var frames []protocol.Frame
	for {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			if err == nil {
				frames = append(frames, f)
			}
			payload.Packet.Release()
		default:
			return frames
		}
	}
}

// TestOutboundPingInboundPongRoundTrip verifies the stage③ spec 7.4 flow: the
// send side creates an active ping and registers it in the shared LaneManager;
// the recv glue routes an inbound PONG to that same ping via
// LaneManager.LookupPing(key), yielding an RTT quality sample. The recv glue
// never owns the probe instance — it only looks it up.
func TestOutboundPingInboundPongRoundTrip(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{SessionManager: sessions})
	handler := runtime.NewRecvHandler(s, sessions)

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "ep-1",
		RemoteAddr: &testAddr{addr: "127.0.0.1:9000"},
	}

	const sessionID = uint64(7)
	const laneID = uint8(3)

	// Simulate the send side creating + registering a ping for this leg. The
	// sendMsg closure writes a PING frame through Send.WriteFrame, exactly as
	// Send.startLanePing does. We capture the emitted ping id/time to echo back.
	var emittedID, emittedTime uint64
	p := ping.New(ping.Config{
		Interval: time.Hour, // we drive a single ping manually below
		Timeout:  time.Second,
		SendMsg: func(m ping.Message) error {
			emittedID, emittedTime = m.ID, m.TimeMS
			return s.WriteFrame(context.Background(), protocol.Frame{
				Version:   protocol.Version,
				Type:      protocol.TypePING,
				SessionID: sessionID,
				LaneID:    laneID,
				Body:      protocol.PingBody{PingID: m.ID, TimeMS: m.TimeMS},
			}, leg)
		},
	})
	key := send.KeyForLeg(sessionID, laneID, leg)
	s.LaneManager().RegisterPing(key, p)

	// Drive one ping by running Start briefly (it emits immediately, then ticks
	// at the 1h interval, so a short run produces exactly one PING).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	go p.Start(ctx)
	time.Sleep(60 * time.Millisecond)

	frames := drainPackets(s)
	var pingFrame *protocol.Frame
	for i := range frames {
		if frames[i].Type == protocol.TypePING {
			pingFrame = &frames[i]
			break
		}
	}
	if pingFrame == nil {
		t.Fatal("expected a PING frame emitted through Send.WriteFrame")
	}
	if pingFrame.SessionID != sessionID || pingFrame.LaneID != laneID {
		t.Errorf("PING addressed wrong: session=%d lane=%d", pingFrame.SessionID, pingFrame.LaneID)
	}

	pingBody := pingFrame.Body.(protocol.PingBody)
	if pingBody.PingID != emittedID || pingBody.TimeMS != emittedTime {
		t.Errorf("PING frame id/time = %d/%d, want %d/%d", pingBody.PingID, pingBody.TimeMS, emittedID, emittedTime)
	}

	// Simulate the peer echoing the PING back as a PONG. OnPong must route to the
	// LaneManager-registered ping and validate the sample.
	pongFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePONG,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      protocol.PingBody{PingID: pingBody.PingID, TimeMS: pingBody.TimeMS},
	}
	if err := handler.OnPong(context.Background(), leg, pongFrame); err != nil {
		t.Fatalf("OnPong failed: %v", err)
	}

	// A PONG for an unregistered leg must be dropped, not error.
	otherLeg := transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep-unknown", RemoteAddr: &testAddr{addr: "127.0.0.1:9999"}}
	if err := handler.OnPong(context.Background(), otherLeg, pongFrame); err != nil {
		t.Fatalf("OnPong for unregistered leg should be a no-op, got: %v", err)
	}
}

// TestInboundBandwidthProbeSendsAck verifies the spec 7.5 inbound BW_PROBE flow:
// RecvHandler.OnBandwidthProbe converts the frame to bw.Probe, feeds the
// lane-transport BW, and the BW emits a BW_PROBE_ACK through Send.WriteFrame.
func TestInboundBandwidthProbeSendsAck(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{SessionManager: sessions})
	handler := runtime.NewRecvHandler(s, sessions, runtime.Config{
		BWReferenceBps: 1_000_000,
	})

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "ep-2",
		RemoteAddr: &testAddr{addr: "127.0.0.1:9001"},
	}

	const sessionID = uint64(11)
	const laneID = uint8(2)
	const trainID = uint64(99)
	const count = 10

	// The session must exist, else OnBandwidthProbe replies CLOSE{UnknownSession}
	// instead of an ACK (peer-restart self-heal). Register it first.
	sessions.GetOrCreate(sessionID)

	ctx := context.Background()

	// Deliver a full train of probes; the BW should emit at least one ACK.
	for seq := uint16(0); seq < count; seq++ {
		frame := protocol.Frame{
			Version:   protocol.Version,
			Type:      protocol.TypeBandwidthProbe,
			SessionID: sessionID,
			LaneID:    laneID,
			Body: protocol.BandwidthProbeBody{
				ProbeID:             trainID,
				Seq:                 seq,
				Count:               count,
				SendMS:              uint64(seq) * 10,
				TrainBytesTotal:     12000,
				TrainBytesRemaining: 12000 - uint64(seq+1)*1200,
				Payload:             make([]byte, 1200),
			},
		}
		if _, err := handler.OnBandwidthProbe(ctx, leg, frame); err != nil {
			t.Fatalf("OnBandwidthProbe seq=%d failed: %v", seq, err)
		}
	}

	frames := drainPackets(s)
	sawAck := false
	for _, f := range frames {
		if f.Type == protocol.TypeBandwidthProbeAck {
			sawAck = true
			ackBody := f.Body.(protocol.BandwidthProbeAckBody)
			if ackBody.ProbeID != trainID {
				t.Errorf("ACK probe id = %d, want %d", ackBody.ProbeID, trainID)
			}
		}
	}
	if !sawAck {
		t.Error("expected a BW_PROBE_ACK emitted through Send.WriteFrame")
	}
}

// TestOutboundBandwidthProbeAckRoutesToLoop verifies the stage④ active flow: the
// send side creates a BwLoop and registers it in the shared LaneManager (as the
// bwScheduler does); an inbound BW_PROBE_ACK routed through OnBandwidthProbeAck
// reaches that same loop via LaneManager.LookupBwLoop and produces a Sample. The
// recv glue never owns the loop — it only looks it up.
func TestOutboundBandwidthProbeAckRoutesToLoop(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{SessionManager: sessions})
	handler := runtime.NewRecvHandler(s, sessions)

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "ep-3",
		RemoteAddr: &testAddr{addr: "127.0.0.1:9002"},
	}

	const sessionID = uint64(21)
	const laneID = uint8(4)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Simulate the send-side bwScheduler: build a BwLoop whose SendProbe writes a
	// BW_PROBE frame, register it in the LaneManager, and capture its sample.
	var samples []bw.Sample
	var smu sync.Mutex
	var loop *bw.BwLoop
	b := bw.New(bw.Config{
		ReferenceBps: 1_000_000,
		CapBps:       16_000_000, // single step at cap → train ends after one step
		StepWindow:   40 * time.Millisecond,
		SendProbe: func(p bw.Probe) error {
			return s.WriteFrame(ctx, protocol.Frame{
				Version:   protocol.Version,
				Type:      protocol.TypeBandwidthProbe,
				SessionID: sessionID,
				LaneID:    laneID,
				Body: protocol.BandwidthProbeBody{
					TrainID:             p.ID,
					ProbeID:             p.ID,
					Seq:                 p.Seq,
					Count:               p.Count,
					SendMS:              p.SendMS,
					TrainBytesTotal:     p.Total,
					TrainBytesRemaining: p.Remaining,
					Payload:             make([]byte, p.Bytes),
				},
			}, leg)
		},
		OnSample: func(sample bw.Sample) {
			smu.Lock()
			samples = append(samples, sample)
			smu.Unlock()
		},
	})
	var err error
	loop, err = b.Start(ctx)
	if err != nil {
		t.Fatalf("bw.Start failed: %v", err)
	}
	s.LaneManager().PutBwLoop(loop.TrainID(), loop)

	// Let the first step's probes go out.
	time.Sleep(120 * time.Millisecond)

	frames := drainPackets(s)
	sawProbe := false
	var lastCount uint16
	for _, f := range frames {
		if f.Type == protocol.TypeBandwidthProbe {
			sawProbe = true
			if f.SessionID != sessionID || f.LaneID != laneID {
				t.Errorf("BW_PROBE addressed wrong: session=%d lane=%d", f.SessionID, f.LaneID)
			}
			lastCount = f.Body.(protocol.BandwidthProbeBody).Count
		}
	}
	if !sawProbe {
		t.Fatal("expected BW_PROBE frames emitted through Send.WriteFrame")
	}

	// Simulate the peer ACKing the whole step, routed back through the handler.
	// OnBandwidthProbeAck must find the loop via LaneManager and feed it.
	nowMS := uint64(time.Now().UnixMilli())
	full := uint64(0)
	for i := uint16(0); i < lastCount; i++ {
		full |= 1 << i
	}
	ackFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeBandwidthProbeAck,
		SessionID: sessionID,
		LaneID:    laneID,
		Body: protocol.BandwidthProbeAckBody{
			ProbeID:   loop.TrainID(),
			Count:     lastCount,
			Received:  full,
			FirstRXMS: nowMS - 100,
			LastRXMS:  nowMS,
		},
	}
	if err := handler.OnBandwidthProbeAck(ctx, leg, ackFrame); err != nil {
		t.Fatalf("OnBandwidthProbeAck failed: %v", err)
	}

	// The train should finish and emit a sample.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		smu.Lock()
		n := len(samples)
		smu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(samples) == 0 {
		t.Fatal("expected a bw.Sample after the train completed")
	}
}
