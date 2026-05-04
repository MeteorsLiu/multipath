package send

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	tunio "github.com/MeteorsLiu/multipath/internal/tun"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
	recvpkg "github.com/MeteorsLiu/multipath/internal/tunnel/recv"
)

func TestSendWriteScheduledFrame(t *testing.T) {
	in := New()
	lane := newLaneRuntime(3, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	laneID, charge, err := in.writeScheduledFrame(context.Background(), protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		Body:      protocol.DataBody{Packet: []byte("packet")},
	})
	if err != nil {
		t.Fatalf("writeScheduledFrame failed: %v", err)
	}
	if laneID != 3 {
		t.Fatalf("laneID = %d, want 3", laneID)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if charge != uint32(len(written.Packet.Payload)) {
		t.Fatalf("charge = %d, want %d", charge, len(written.Packet.Payload))
	}

	got, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode written payload: %v", err)
	}
	if got.LaneID != 3 {
		t.Fatalf("written LaneID = %d, want 3", got.LaneID)
	}
	if got.Type != protocol.TypeDATA {
		t.Fatalf("written Type = %d, want DATA", got.Type)
	}
}

func TestSendWriteScheduledFrameNoRunnableLane(t *testing.T) {
	in := New()
	_, _, err := in.writeScheduledFrame(context.Background(), protocol.Frame{
		Type: protocol.TypeDATA,
		Body: protocol.DataBody{},
	})
	if !errors.Is(err, errNoRunnableLane) {
		t.Fatalf("err = %v, want errNoRunnableLane", err)
	}
}

func TestSendWriteScheduledFrameIgnoresOtherSessionLane(t *testing.T) {
	in := New()
	lane := newLaneRuntime(9, 1)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 100, laneID: 9}] = lane

	_, _, err := in.writeScheduledFrame(context.Background(), protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		Body:      protocol.DataBody{},
	})
	if !errors.Is(err, errNoRunnableLane) {
		t.Fatalf("err = %v, want errNoRunnableLane", err)
	}
}

func TestSendWriteScheduledFrameSkipsUnavailableLaneAndUsesNext(t *testing.T) {
	in := New()
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = newLaneRuntime(1, 1)
	lane := newLaneRuntime(2, 1)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 2}] = lane

	laneID, _, err := in.writeScheduledFrame(context.Background(), protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		Body:      protocol.DataBody{},
	})
	if err != nil {
		t.Fatalf("writeScheduledFrame failed: %v", err)
	}
	if laneID != 2 {
		t.Fatalf("laneID = %d, want 2", laneID)
	}
}

func TestSendRunnableLanesCacheInvalidatesWhenDirty(t *testing.T) {
	in := New()
	lane1 := newLaneRuntime(1, 1)
	lane1.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = lane1

	got := in.runnableLanes(99)
	if len(got) != 1 || got[0].id != 1 {
		t.Fatalf("initial runnable lanes = %+v, want lane 1", got)
	}

	lane2 := newLaneRuntime(2, 1)
	lane2.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:2234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 2}] = lane2
	in.markRunnableLanesDirty(99)

	got = in.runnableLanes(99)
	if len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
		t.Fatalf("dirty rebuilt runnable lanes = %+v, want lanes 1,2", got)
	}

	lane1.udpReady = false
	in.markRunnableLanesDirty(99)
	got = in.runnableLanes(99)
	if len(got) != 1 || got[0].id != 2 {
		t.Fatalf("after lane loss runnable lanes = %+v, want lane 2", got)
	}
}

func TestSendWritePacketAllocatesPacketIDAfterSuccessfulWrite(t *testing.T) {
	in := New()
	lane := newLaneRuntime(3, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	packetID, laneID, _, err := in.writeTUNPacket(context.Background(), 99, []byte("ip-packet"))
	if err != nil {
		t.Fatalf("writeTUNPacket failed: %v", err)
	}
	if packetID != 0 {
		t.Fatalf("packetID = %d, want 0", packetID)
	}
	if laneID != 3 {
		t.Fatalf("laneID = %d, want 3", laneID)
	}

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode written payload: %v", err)
	}
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		t.Fatalf("body type = %T, want DataBody", frame.Body)
	}
	if body.PacketID != 0 {
		t.Fatalf("written packetID = %d, want 0", body.PacketID)
	}
	if string(body.Packet) != "ip-packet" {
		t.Fatalf("written packet = %q, want ip-packet", body.Packet)
	}
	if len(mustSendState(t, in, 99).txWindow.pending) != 0 {
		t.Fatal("txWindow stored packet while FEC is off")
	}
	in.enableFEC()
	if _, _, _, err := in.writeTUNPacket(context.Background(), 99, []byte("fec-packet")); err != nil {
		t.Fatalf("writeTUNPacket with FEC failed: %v", err)
	}
	if len(mustSendState(t, in, 99).txWindow.pending) != 1 {
		t.Fatalf("txWindow pending = %d, want 1", len(mustSendState(t, in, 99).txWindow.pending))
	}
}

func TestSendWritePacketDoesNotAdvancePacketIDOnFailure(t *testing.T) {
	in := New()
	session := mustSendState(t, in, 99)

	packetID, _, _, err := in.writeTUNPacket(context.Background(), 99, []byte("ip-packet"))
	if !errors.Is(err, errNoRunnableLane) {
		t.Fatalf("writeTUNPacket err = %v, want errNoRunnableLane", err)
	}
	if packetID != 0 {
		t.Fatalf("packetID = %d, want 0", packetID)
	}
	if got := session.nextPacketID.Load(); got != 0 {
		t.Fatalf("nextPacketID = %d, want 0", got)
	}
}

func TestSendWritePacketDoesNotHardStopAtMaxPacketID(t *testing.T) {
	in := New()
	session := mustSendState(t, in, 99)
	session.nextPacketID.Store(^uint32(0))
	lane := newLaneRuntime(3, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	packetID, _, _, err := in.writeTUNPacket(context.Background(), 99, []byte("ip-packet"))
	if err != nil {
		t.Fatalf("writeTUNPacket failed: %v", err)
	}
	if packetID != ^uint32(0) {
		t.Fatalf("packetID = %d, want max uint32", packetID)
	}
	written := readSendPayload(t, in)
	written.Packet.Release()
	if got := session.nextPacketID.Load(); got != 0 {
		t.Fatalf("nextPacketID = %d, want 0 after uint32 wrap", got)
	}
}

func TestSendWritePacketSendsRepairAfterFECGroup(t *testing.T) {
	in := New()
	in.enableFEC()
	session := mustSendState(t, in, 99)
	in.fecCodec = &fakeFECCodec{
		encodeFunc: func(shards [][]byte, key uint16) error {
			shards[len(shards)-1] = []byte("repair")
			return nil
		},
	}
	lane := newLaneRuntime(3, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	for i := 0; i < 4; i++ {
		if _, _, _, err := in.writeTUNPacket(context.Background(), 99, []byte{byte('a' + i)}); err != nil {
			t.Fatalf("writeTUNPacket %d failed: %v", i, err)
		}
	}

	var written transport.Payload
	for i := 0; i < 5; i++ {
		written = readSendPayload(t, in)
		if i < 4 {
			written.Packet.Release()
		}
	}
	defer written.Packet.Release()
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode REPAIR: %v", err)
	}
	if frame.Type != protocol.TypeREPAIR {
		t.Fatalf("frame type = %d, want REPAIR", frame.Type)
	}
	repair, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		t.Fatalf("body type = %T, want RepairBody", frame.Body)
	}
	if repair.BasePacketID != 0 || repair.Key != 0 || repair.SourceSpan != 4 || string(repair.Symbol) != "repair" {
		t.Fatalf("REPAIR = base %d key %d sourceSpan %d symbol %q", repair.BasePacketID, repair.Key, repair.SourceSpan, repair.Symbol)
	}
	if len(session.txWindow.pending) != 0 {
		t.Fatalf("txWindow pending = %d, want 0", len(session.txWindow.pending))
	}
}

func TestSendFECFlushTimerSendsPartialRepair(t *testing.T) {
	in := New(Config{FECFlushFixedMs: 1})
	in.enableFEC()
	in.fecCodecs[1] = &fakeFECCodec{
		encodeFunc: func(shards [][]byte, key uint16) error {
			if len(shards) != 2 {
				t.Fatalf("shards = %d, want 2", len(shards))
			}
			shards[len(shards)-1] = []byte("repair")
			return nil
		},
	}
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 10)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	if _, _, _, err := in.writeTUNPacket(context.Background(), 99, []byte("a")); err != nil {
		t.Fatalf("writeTUNPacket failed: %v", err)
	}
	data := readSendPayload(t, in)
	data.Packet.Release()

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode REPAIR: %v", err)
	}
	repair, ok := frame.Body.(protocol.RepairBody)
	if frame.Type != protocol.TypeREPAIR || !ok {
		t.Fatalf("frame = type %d body %T, want REPAIR", frame.Type, frame.Body)
	}
	if repair.BasePacketID != 0 || repair.Key != 0 || repair.SourceSpan != 1 || string(repair.Symbol) != "repair" {
		t.Fatalf("REPAIR = base %d key %d sourceSpan %d symbol %q", repair.BasePacketID, repair.Key, repair.SourceSpan, repair.Symbol)
	}
}

func TestSendStartLaneSendsHELLOWithoutEnqueue(t *testing.T) {
	in := New()

	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:       transport.KindUDP,
			EndpointID: "udp0",
			RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
		},
		Caps:       3,
		FECProfile: 1,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.EndpointID != "udp0" || written.Leg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("HELLO leg = %+v, want udp0/127.0.0.1:1234", written.Leg)
	}

	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode HELLO: %v", err)
	}
	if frame.Type != protocol.TypeHELLO {
		t.Fatalf("frame type = %d, want HELLO", frame.Type)
	}
	hello, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		t.Fatalf("body type = %T, want HelloBody", frame.Body)
	}
	if hello.Nonce != 0 || hello.Caps != 3 || hello.FECProfile != 1 {
		t.Fatalf("HELLO body = %+v", hello)
	}

	lane := in.lanes[laneKey{sessionID: 99, laneID: 3}]
	if lane == nil {
		t.Fatal("lane was not created")
	}
	route, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]
	if !ok || !route.valid() || route.nonce() != 0 {
		t.Fatalf("hello route = (%v,%v,%d), want valid nonce 0", ok, route.valid(), route.nonce())
	}
	if len(route.payload) == 0 || route.leg.EndpointID != "udp0" {
		t.Fatalf("hello route not stored: leg=%+v payload=%d", route.leg, len(route.payload))
	}
	if lane.udpLeg.EndpointID != "udp0" || lane.udpLeg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("udp probe leg = %+v, want udp0/127.0.0.1:1234", lane.udpLeg)
	}
	if lane.udpReady {
		t.Fatal("udpReady = true before HELLO_ACK")
	}
	if lane.ready() {
		t.Fatal("lane was ready before HELLO_ACK")
	}
	if got := in.runnableLanes(99); len(got) != 0 {
		t.Fatalf("runnable lanes before HELLO_ACK = %d, want 0", len(got))
	}
}

func TestSendRetriesHELLOOnProbeTick(t *testing.T) {
	in := New()
	in.probeInterval = time.Second
	in.probeTimeout = 3 * time.Second

	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:       transport.KindUDP,
			EndpointID: "udp0",
			RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
		},
		Caps: protocol.CapTCPFallback,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()

	if err := in.retryOpenHELLO(context.Background(), 1000); err != nil {
		t.Fatalf("retryOpenHELLO failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()
	route := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]
	if route.lastRetryMS != 1000 {
		t.Fatalf("hello last retry = %d, want 1000", route.lastRetryMS)
	}

	if err := in.retryOpenHELLO(context.Background(), 1200); err != nil {
		t.Fatalf("retryOpenHELLO second failed: %v", err)
	}
	assertNoSendPayload(t, in)
}

func TestSendHELLOTimeoutStartsTCPFallback(t *testing.T) {
	streamTransport := &fakeStreamTransport{
		dialLeg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
	}
	in := New()
	in.streamTransport = streamTransport
	in.probeInterval = time.Second
	in.probeTimeout = time.Second
	probeEvents := make(chan probe.Event, 1)
	in.probeEvents = probeEvents

	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:       transport.KindUDP,
			EndpointID: "udp0",
			RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
		},
		TCPRemote:  "127.0.0.1:4321",
		Caps:       protocol.CapTCPFallback | protocol.CapFEC,
		FECProfile: protocol.FECProfileSLC4Plus1,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()

	if err := in.retryOpenHELLO(context.Background(), 1000); err != nil {
		t.Fatalf("retryOpenHELLO failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()
	if err := in.retryOpenHELLO(context.Background(), 2000); err != nil {
		t.Fatalf("retryOpenHELLO timeout failed: %v", err)
	}

	lane := in.lanes[laneKey{sessionID: 99, laneID: 3}]
	if lane.udpLeg.EndpointID != "udp0" {
		t.Fatalf("udp leg was not retained for recovery probes: %+v", lane.udpLeg)
	}
	if lane.udpReady {
		t.Fatal("udpReady = true after HELLO timeout")
	}
	select {
	case event := <-probeEvents:
		if event.Type != probe.EventTrack || event.Target == 0 {
			t.Fatalf("probe event = %+v, want EventTrack with target", event)
		}
	default:
		t.Fatal("UDP leg was not tracked for recovery probe")
	}

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.Kind != transport.KindTCP || written.Leg.ConnID != "tcp0" {
		t.Fatalf("fallback leg = %+v, want TCP tcp0", written.Leg)
	}
	if got := streamTransport.dialed[0]; got != "127.0.0.1:4321" {
		t.Fatalf("dialed remote = %q, want 127.0.0.1:4321", got)
	}

	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode TCP HELLO: %v", err)
	}
	hello, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		t.Fatalf("body type = %T, want HelloBody", frame.Body)
	}
	if hello.Caps != (protocol.CapTCPFallback|protocol.CapFEC) || hello.FECProfile != protocol.FECProfileSLC4Plus1 {
		t.Fatalf("TCP fallback HELLO body = %+v", hello)
	}
}

func TestSendTCPHELLOTimeoutUsesInitialRTO(t *testing.T) {
	streamTransport := &fakeStreamTransport{}
	in := New()
	in.streamTransport = streamTransport
	in.probeInterval = 200 * time.Millisecond
	in.probeTimeout = 100 * time.Millisecond

	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
		Caps:       protocol.CapTCPFallback,
		FECProfile: protocol.FECProfileOff,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()

	if err := in.retryOpenHELLO(context.Background(), 1000); err != nil {
		t.Fatalf("retryOpenHELLO failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()
	if err := in.retryOpenHELLO(context.Background(), 1099); err != nil {
		t.Fatalf("retryOpenHELLO before TCP RTO failed: %v", err)
	}
	if _, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]; !ok {
		t.Fatal("TCP HELLO route timed out using probeTimeout instead of initial RTO")
	}

	if err := in.retryOpenHELLO(context.Background(), 2000); err != nil {
		t.Fatalf("retryOpenHELLO timeout failed: %v", err)
	}
	if _, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]; ok {
		t.Fatal("TCP HELLO route still exists after initial RTO")
	}
	if got := streamTransport.closed["tcp0"]; got != 1 {
		t.Fatalf("closed tcp0 count = %d, want 1", got)
	}
}

func TestSendTCPHELLOTimeoutUsesRTTEstimate(t *testing.T) {
	in := New()
	in.probeTimeout = 600 * time.Millisecond
	key := laneKey{sessionID: 99, laneID: 3}
	lane := newLaneRuntime(3, 10)
	lane.rttTCP.Add(800)
	in.lanes[key] = lane

	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
		Caps:       protocol.CapTCPFallback,
		FECProfile: protocol.FECProfileOff,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()

	route := in.helloRoutes[key]
	if route.timeout != 2400*time.Millisecond {
		t.Fatalf("TCP HELLO timeout = %s, want 2.4s", route.timeout)
	}
}

func TestSendTCPHELLOTimeoutUsesSessionRTTEstimate(t *testing.T) {
	in := New()
	in.probeTimeout = 600 * time.Millisecond
	udpLane := newLaneRuntime(1, 10)
	udpLane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	udpLane.rttUDP.Add(1500)
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = udpLane

	tcpKey := laneKey{sessionID: 99, laneID: 3}
	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
		Weight:  10,
		Leg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
		Caps:       protocol.CapTCPFallback,
		FECProfile: protocol.FECProfileOff,
	})
	if err != nil {
		t.Fatalf("startLane failed: %v", err)
	}
	readSendPayload(t, in).Packet.Release()

	route := in.helloRoutes[tcpKey]
	if route.timeout != 1600*time.Millisecond {
		t.Fatalf("TCP HELLO timeout = %s, want 1.6s", route.timeout)
	}
}

func TestSendRetryFallbackDialsAfterDialError(t *testing.T) {
	dialErr := errors.New("dial failed")
	streamTransport := &fakeStreamTransport{dialErr: dialErr}
	in := New()
	in.streamTransport = streamTransport
	in.negotiatedCaps.Store(uint32(protocol.CapTCPFallback))
	mustSendState(t, in, 99)
	in.activateSession(99)
	lane := newLaneRuntime(3, 10)
	lane.setTCPRemote("127.0.0.1:4321")
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	in.retryFallbackDials(context.Background())
	waitForDialCount(t, streamTransport, 1)
	if lane.fallbackDialing {
		t.Fatal("fallbackDialing = true after failed dial result")
	}

	in.retryFallbackDials(context.Background())
	time.Sleep(10 * time.Millisecond)
	if len(streamTransport.dialed) != 1 {
		t.Fatalf("fallback dials = %d, want 1 during backoff", len(streamTransport.dialed))
	}

	lane.mu.Lock()
	lane.fallbackRetryAt = time.Now().Add(-time.Millisecond)
	lane.mu.Unlock()
	in.retryFallbackDials(context.Background())
	waitForDialCount(t, streamTransport, 2)
}

func TestSendStartLaneRejectsZeroWeight(t *testing.T) {
	in := New()
	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  3,
	})
	if !errors.Is(err, errInvalidLane) {
		t.Fatalf("err = %v, want errInvalidLane", err)
	}
}

func TestSendStartLaneRejectsSessionControlLaneID(t *testing.T) {
	in := New()
	err := in.startLane(context.Background(), startLaneConfig{
		Session: mustSession(t, in, 99),
		LaneID:  protocol.SessionControlLaneID,
		Weight:  1,
	})
	if !errors.Is(err, errInvalidLane) {
		t.Fatalf("err = %v, want errInvalidLane", err)
	}
}

func TestSendConfigBootstrapsLanes(t *testing.T) {
	in := New(Config{
		ProbeInterval: time.Second,
		ProbeTimeout:  2 * time.Second,
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 3,
				Weight: 10,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "udp0",
					RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
				},
				TCPRemote: "127.0.0.1:4321",
			},
		},
	})

	if err := in.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if in.probeInterval != time.Second || in.probeTimeout != 2*time.Second {
		t.Fatalf("probe config = interval %s timeout %s", in.probeInterval, in.probeTimeout)
	}
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode HELLO: %v", err)
	}
	if frame.Type != protocol.TypeHELLO || frame.SessionID == 0 || frame.LaneID != 3 {
		t.Fatalf("bootstrap frame route = type %d session %d lane %d", frame.Type, frame.SessionID, frame.LaneID)
	}
	lane := in.lanes[laneKey{sessionID: frame.SessionID, laneID: 3}]
	if lane == nil {
		t.Fatal("bootstrap lane was not created")
	}
	if lane.tcpRemote != "127.0.0.1:4321" {
		t.Fatalf("tcpRemote = %q, want 127.0.0.1:4321", lane.tcpRemote)
	}

	if err := in.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap failed: %v", err)
	}
	assertNoSendPayload(t, in)
}

func TestSendRepliesCloseForUnknownSessionPING(t *testing.T) {
	in := New()
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	if err := in.receivePing(context.Background(), 99, 3, leg, protocol.PingBody{PingID: 7, TimeMS: 1000}); err != nil {
		t.Fatalf("receivePing failed: %v", err)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode CLOSE: %v", err)
	}
	body, ok := frame.Body.(protocol.CloseBody)
	if frame.Type != protocol.TypeCLOSE || !ok {
		t.Fatalf("frame = type %d body %T, want CLOSE", frame.Type, frame.Body)
	}
	if frame.SessionID != 99 || frame.LaneID != protocol.SessionControlLaneID {
		t.Fatalf("CLOSE route = session %d lane %d", frame.SessionID, frame.LaneID)
	}
	if body.Scope != protocol.CloseScopeSession || body.Reason != protocol.CloseReasonUnknownSession {
		t.Fatalf("CLOSE body = %+v", body)
	}
}

func TestSendRebootstrapsOnUnknownSessionCLOSE(t *testing.T) {
	in := New(Config{
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 3,
				Weight: 10,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "udp0",
					RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
				},
			},
		},
	})
	if err := in.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	first := readSendPayload(t, in)
	defer first.Packet.Release()
	firstFrame, err := protocol.Decode(first.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode first HELLO: %v", err)
	}
	if firstFrame.SessionID == 0 {
		t.Fatal("first HELLO session = 0")
	}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: firstFrame.SessionID,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.CloseBody{
			Scope:  protocol.CloseScopeSession,
			Reason: protocol.CloseReasonUnknownSession,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode CLOSE: %v", err)
	}
	if err := writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("recv Write CLOSE session failed: %v", err)
	}
	second := readSendPayload(t, in)
	defer second.Packet.Release()
	secondFrame, err := protocol.Decode(second.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode second HELLO: %v", err)
	}
	if secondFrame.Type != protocol.TypeHELLO || secondFrame.LaneID != 3 {
		t.Fatalf("second frame = type %d lane %d, want HELLO lane 3", secondFrame.Type, secondFrame.LaneID)
	}
	if secondFrame.SessionID == 0 || secondFrame.SessionID == firstFrame.SessionID {
		t.Fatalf("second session = %d, first = %d; want fresh non-zero session", secondFrame.SessionID, firstFrame.SessionID)
	}
	if getSendState(in, firstFrame.SessionID) != nil {
		t.Fatal("old session still exists after unknown_session CLOSE")
	}
	if getSendState(in, secondFrame.SessionID) == nil {
		t.Fatal("new session was not created after unknown_session CLOSE")
	}
}

func TestSendIgnoresDuplicateUnknownSessionCLOSE(t *testing.T) {
	in := New(Config{
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 3,
				Weight: 10,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "udp0",
					RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
				},
			},
		},
	})
	if err := in.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	first := readSendPayload(t, in)
	firstFrame, err := protocol.Decode(first.Packet.Payload)
	first.Packet.Release()
	if err != nil {
		t.Fatalf("Decode first HELLO: %v", err)
	}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: firstFrame.SessionID,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.CloseBody{
			Scope:  protocol.CloseScopeSession,
			Reason: protocol.CloseReasonUnknownSession,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode CLOSE: %v", err)
	}
	if err := writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("first CLOSE failed: %v", err)
	}
	second := readSendPayload(t, in)
	second.Packet.Release()

	if err := writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("duplicate CLOSE failed: %v", err)
	}
	select {
	case payload := <-in.Packets():
		payload.Packet.Release()
		t.Fatalf("duplicate unknown-session CLOSE emitted extra payload on leg %+v", payload.Leg)
	default:
	}
}

func TestSendDeduplicatesConcurrentUnknownSessionCLOSE(t *testing.T) {
	in := New(Config{
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 3,
				Weight: 10,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "udp0",
					RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
				},
			},
		},
	})
	if err := in.bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	first := readSendPayload(t, in)
	firstFrame, err := protocol.Decode(first.Packet.Payload)
	first.Packet.Release()
	if err != nil {
		t.Fatalf("Decode first HELLO: %v", err)
	}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: firstFrame.SessionID,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.CloseBody{
			Scope:  protocol.CloseScopeSession,
			Reason: protocol.CloseReasonUnknownSession,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode CLOSE: %v", err)
	}

	const workers = 16
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errCh <- writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload))
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent CLOSE failed: %v", err)
		}
	}

	var bootstraps int
	for {
		select {
		case payload := <-in.Packets():
			bootstraps++
			payload.Packet.Release()
		default:
			if bootstraps != 1 {
				t.Fatalf("rebootstrap payloads = %d, want 1", bootstraps)
			}
			return
		}
	}
}

func TestRecvHandleDATAWritesTUN(t *testing.T) {
	tun := &recordTUNWriter{}
	out := newTestRecv(t, nil)

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 7, Packet: []byte("ip-packet")},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	event := testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload)
	err = out.WriteTo(context.Background(), event.Leg, event.Packet)
	if err != nil {
		t.Fatalf("recv Write failed: %v", err)
	}
	writeRecvPacketToTUN(t, out, tun)
	clear(payload)
	if string(tun.packet) != "ip-packet" {
		t.Fatalf("tun packet = %q, want ip-packet", tun.packet)
	}
}

func TestRecvHandleDATAWritesWithoutControlAccept(t *testing.T) {
	tun := &recordTUNWriter{}
	out := newTestRecv(t, nil)

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 7, Packet: []byte("ip-packet")},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if err := writeRecvPayload(context.Background(), out, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("recv Write failed: %v", err)
	}
	writeRecvPacketToTUN(t, out, tun)
	if string(tun.packet) != "ip-packet" {
		t.Fatalf("tun packet = %q, want ip-packet", tun.packet)
	}
}

func TestRecvHandleDropsInvalidFrame(t *testing.T) {
	out := newTestRecv(t, nil)

	if err := writeRecvPayload(context.Background(), out, testEvent(transport.LegRef{}, []byte{0x10})); err != nil {
		t.Fatalf("recv Write invalid frame err = %v, want nil", err)
	}
	assertNoRecvPacket(t, out)
}

func TestRecvHandleDropsInvalidBody(t *testing.T) {
	out := newTestRecv(t, nil)

	payload := []byte{protocol.Version<<4 | uint8(protocol.TypeDATA), 0, 0, 0, 0, 0, 0, 0, 99, 3, 1, 2, 3}

	if err := writeRecvPayload(context.Background(), out, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("recv Write invalid body err = %v, want nil", err)
	}
	assertNoRecvPacket(t, out)
}

func TestSendHandleHELLOCreatesLaneAndRepliesOnObservedLeg(t *testing.T) {
	in := New(Config{EnableFEC: true})

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLO,
		SessionID: 99,
		LaneID:    3,
		Body: protocol.HelloBody{
			Nonce:      123,
			Caps:       3,
			FECProfile: 1,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload))
	if err != nil {
		t.Fatalf("recv Write HELLO failed: %v", err)
	}

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.EndpointID != "udp0" || written.Leg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("reply leg = %+v, want udp0/127.0.0.1:1234", written.Leg)
	}

	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode HELLO_ACK: %v", err)
	}
	if frame.Type != protocol.TypeHELLOACK {
		t.Fatalf("reply type = %d, want HELLO_ACK", frame.Type)
	}
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok {
		t.Fatalf("body type = %T, want HelloAckBody", frame.Body)
	}
	wantCaps := protocol.CapTCPFallback | protocol.CapFEC
	if body.Nonce != 123 || body.Accepted != 1 || body.Caps != wantCaps || body.FECProfile != protocol.FECProfileSLC4Plus1 {
		t.Fatalf("HELLO_ACK body = %+v", body)
	}
	if uint16(in.negotiatedCaps.Load()) != wantCaps || uint8(in.fecProfile.Load()) != protocol.FECProfileSLC4Plus1 {
		t.Fatalf("negotiated = (%#x,%d), want (%#x,%d)", uint16(in.negotiatedCaps.Load()), uint8(in.fecProfile.Load()), wantCaps, protocol.FECProfileSLC4Plus1)
	}

	lane := in.lanes[laneKey{sessionID: 99, laneID: 3}]
	if lane == nil {
		t.Fatal("lane was not created")
	}
	if !lane.udpReady {
		t.Fatal("created lane udpReady = false, want true")
	}
	if got := in.runnableLanes(99); len(got) != 1 || got[0].id != 3 {
		t.Fatalf("runnable lanes = %+v, want lane 3", got)
	}
}

func TestSendHandleHELLORejectsSessionControlLaneID(t *testing.T) {
	in := New()

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLO,
		SessionID: 99,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.HelloBody{
			Nonce:      123,
			Caps:       protocol.CapTCPFallback,
			FECProfile: protocol.FECProfileOff,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload))
	if err != nil {
		t.Fatalf("recv Write HELLO failed: %v", err)
	}
	if _, ok := in.lanes[laneKey{sessionID: 99, laneID: protocol.SessionControlLaneID}]; ok {
		t.Fatal("session control lane was created")
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode HELLO_ACK: %v", err)
	}
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok {
		t.Fatalf("body type = %T, want HelloAckBody", frame.Body)
	}
	if body.Nonce != 123 || body.Accepted != 0 {
		t.Fatalf("HELLO_ACK body = %+v, want rejected nonce=123", body)
	}
}

func TestSendHandleHELLOACKMarksLaneReady(t *testing.T) {
	in := New(Config{EnableFEC: true})
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	nonce := startTestHELLORoute(t, in, 99, 3)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLOACK,
		SessionID: 99,
		LaneID:    3,
		Body: protocol.HelloAckBody{
			Nonce:      nonce,
			Accepted:   1,
			Caps:       protocol.CapTCPFallback | protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload))
	if err != nil {
		t.Fatalf("recv Write HELLO_ACK failed: %v", err)
	}
	if !lane.udpReady {
		t.Fatal("lane udpReady = false, want true")
	}
	if _, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]; ok {
		t.Fatal("hello route still exists after HELLO_ACK")
	}
	wantCaps := protocol.CapTCPFallback | protocol.CapFEC
	if uint16(in.negotiatedCaps.Load()) != wantCaps || uint8(in.fecProfile.Load()) != protocol.FECProfileSLC4Plus1 {
		t.Fatalf("negotiated = (%#x,%d), want (%#x,%d)", uint16(in.negotiatedCaps.Load()), uint8(in.fecProfile.Load()), wantCaps, protocol.FECProfileSLC4Plus1)
	}
	if got := in.runnableLanes(99); len(got) != 1 || got[0].id != 3 {
		t.Fatalf("runnable lanes = %+v, want lane 3", got)
	}
}

func TestSendHandleHELLOACKUntracksReplacedTCPTarget(t *testing.T) {
	in := New()
	in.probeEvents = make(chan probe.Event, 8)
	mustSendState(t, in, 99)

	oldLeg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-old"}
	newLeg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-new"}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(oldLeg)
	startTestHELLORoute(t, in, 99, 3)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	in.trackProbeTarget(context.Background(), 99, 3, oldLeg)
	readProbeEvent(t, in.probeEvents)

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLOACK,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.HelloAckBody{Nonce: 0, Accepted: 1, Caps: protocol.CapTCPFallback},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := writeTestControl(context.Background(), in, testEvent(newLeg, payload)); err != nil {
		t.Fatalf("recv Write HELLO_ACK failed: %v", err)
	}

	if _, ok := in.probeKeys[newPingKey(oldLeg)]; ok {
		t.Fatal("old TCP probe target still tracked")
	}
	if _, ok := in.probeKeys[newPingKey(newLeg)]; !ok {
		t.Fatal("new TCP probe target was not tracked")
	}
	if !lane.tcpReady || lane.tcpLeg.ConnID != "tcp-new" {
		t.Fatalf("tcp leg = ready %t leg %+v, want tcp-new ready", lane.tcpReady, lane.tcpLeg)
	}
}

func TestSendHandleHELLOACKDropsNonceMismatch(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	startTestHELLORoute(t, in, 99, 3)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLOACK,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.HelloAckBody{Nonce: 456, Accepted: 1},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload))
	if err != nil {
		t.Fatalf("recv Write HELLO_ACK failed: %v", err)
	}
	if lane.udpReady {
		t.Fatal("lane udpReady = true, want false")
	}
	if _, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]; !ok {
		t.Fatal("hello route was removed after nonce mismatch")
	}
	if got := in.runnableLanes(99); len(got) != 0 {
		t.Fatalf("runnable lanes after nonce mismatch = %d, want 0", len(got))
	}
}

func TestSendHandlePINGRepliesWithPONGOnObservedLeg(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypePING,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.PingBody{PingID: 9, TimeMS: 12345},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload))
	if err != nil {
		t.Fatalf("recv Write PING failed: %v", err)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.EndpointID != "udp0" || written.Leg.RemoteAddr.String() != "127.0.0.1:1234" {
		t.Fatalf("reply leg = %+v, want udp0/127.0.0.1:1234", written.Leg)
	}
	if lane.udpReady {
		t.Fatal("PING marked UDP ready; readiness must be driven by HELLO_ACK or probe recovery")
	}
	if got := in.runnableLanes(99); len(got) != 0 {
		t.Fatalf("runnable lanes after PING = %d, want 0", len(got))
	}

	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode PONG: %v", err)
	}
	if frame.Type != protocol.TypePONG {
		t.Fatalf("reply type = %d, want PONG", frame.Type)
	}
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		t.Fatalf("body type = %T, want PingBody", frame.Body)
	}
	if body.PingID != 9 || body.TimeMS != 12345 {
		t.Fatalf("PONG body = %+v, want pingID=9 timeMS=12345", body)
	}
}

func TestSendHandlePONGEmitsProbeEvent(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	probeEvents := make(chan probe.Event, 4)
	in.probeEvents = probeEvents
	lane := newLaneRuntime(3, 1)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	in.trackProbeTarget(context.Background(), 99, 3, leg)
	select {
	case event := <-probeEvents:
		if event.Type != probe.EventTrack {
			t.Fatalf("probe event = %+v, want EventTrack", event)
		}
	default:
		t.Fatal("trackProbeTarget did not emit EventTrack")
	}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypePONG,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.PingBody{PingID: 9, TimeMS: 12345},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(leg, payload))
	if err != nil {
		t.Fatalf("recv Write PONG failed: %v", err)
	}

	select {
	case event := <-probeEvents:
		if event.Type != probe.EventPongReceived || event.PingID != 9 || event.TimeMS != 12345 {
			t.Fatalf("probe event = %+v, want PONG pingID=9 timeMS=12345", event)
		}
	default:
		t.Fatal("PONG did not emit probe event")
	}
}

func TestSendHandlePONGDropsUnmatchedPING(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypePONG,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.PingBody{PingID: 9, TimeMS: 12345},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	err = writeTestControl(context.Background(), in, testEvent(leg, payload))
	if err != nil {
		t.Fatalf("recv Write PONG failed: %v", err)
	}
	if lane.udpReady {
		t.Fatal("lane udpReady = true, want false")
	}
	if got := in.runnableLanes(99); len(got) != 0 {
		t.Fatalf("runnable lanes after unmatched PONG = %d, want 0", len(got))
	}
}

func TestSendSendPINGUsesSpecifiedLeg(t *testing.T) {
	in := New()
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	lane.bindLeg(transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	err := in.sendPING(context.Background(), 99, 3, transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	}, 9, 12345)
	if err != nil {
		t.Fatalf("SendPING failed: %v", err)
	}
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.Kind != transport.KindTCP || written.Leg.ConnID != "tcp0" {
		t.Fatalf("leg = %+v, want TCP tcp0", written.Leg)
	}

	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode PING: %v", err)
	}
	if frame.Type != protocol.TypePING {
		t.Fatalf("frame type = %d, want PING", frame.Type)
	}
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		t.Fatalf("body type = %T, want PingBody", frame.Body)
	}
	if body.PingID != 9 || body.TimeMS != 12345 {
		t.Fatalf("PING body = %+v, want pingID=9 timeMS=12345", body)
	}
}

func TestSendHandleProbeEventSendsPING(t *testing.T) {
	in := New()
	lane := newLaneRuntime(3, 1)
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane.bindLeg(udpLeg)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	in.probeTargets = map[probe.Target]probeBinding{
		1: {sessionID: 99, laneID: 3, leg: udpLeg},
	}

	err := in.handleProbeEvent(context.Background(), probe.Event{
		Type:   probe.EventSendPing,
		Target: 1,
		PingID: 7,
		TimeMS: 12345,
	})
	if err != nil {
		t.Fatalf("handleProbeEvent failed: %v", err)
	}

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	udpFrame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode UDP PING: %v", err)
	}
	if udpFrame.Type != protocol.TypePING || udpFrame.SessionID != 99 || udpFrame.LaneID != 3 {
		t.Fatalf("udp frame route = type %d session %d lane %d", udpFrame.Type, udpFrame.SessionID, udpFrame.LaneID)
	}
	body, ok := udpFrame.Body.(protocol.PingBody)
	if !ok {
		t.Fatalf("body type = %T, want PingBody", udpFrame.Body)
	}
	if body.PingID != 7 || body.TimeMS != 12345 {
		t.Fatalf("PING body = %+v, want pingID=7 timeMS=12345", body)
	}
}

func TestSendProbeTimeoutStartsTCPFallbackHELLO(t *testing.T) {
	streamTransport := &fakeStreamTransport{
		dialLeg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
	}
	in := New()
	in.streamTransport = streamTransport
	in.probeTimeout = 500 * time.Millisecond
	mustSendState(t, in, 99)
	in.negotiatedCaps.Store(uint32(protocol.CapTCPFallback))
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(udpLeg)
	lane.tcpRemote = "127.0.0.1:4321"
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	target := probe.Target(1)
	in.probeTargets = map[probe.Target]probeBinding{
		target: {sessionID: 99, laneID: 3, leg: udpLeg},
	}

	if err := in.handleProbeEvent(context.Background(), probe.Event{Type: probe.EventTargetLost, Target: target}); err != nil {
		t.Fatalf("handleProbeEvent failed: %v", err)
	}
	if lane.udpReady {
		t.Fatal("udpReady = true, want false after timeout")
	}

	written := readSendPayload(t, in)
	defer written.Packet.Release()
	if written.Leg.Kind != transport.KindTCP || written.Leg.ConnID != "tcp0" {
		t.Fatalf("fallback leg = %+v, want TCP tcp0", written.Leg)
	}
	if got := streamTransport.dialed[0]; got != "127.0.0.1:4321" {
		t.Fatalf("dialed remote = %q, want 127.0.0.1:4321", got)
	}

	if !lane.fallbackDialing {
		t.Fatal("fallbackDialing = false after dial result; must stay true until HELLO_ACK")
	}
	if lane.tcpReady {
		t.Fatal("tcpReady = true before TCP HELLO_ACK")
	}
	if _, ok := in.helloRoutes[laneKey{sessionID: 99, laneID: 3}]; !ok {
		t.Fatal("hello route missing before TCP HELLO_ACK")
	}
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode TCP HELLO: %v", err)
	}
	if frame.Type != protocol.TypeHELLO || frame.SessionID != 99 || frame.LaneID != 3 {
		t.Fatalf("fallback frame route = type %d session %d lane %d", frame.Type, frame.SessionID, frame.LaneID)
	}
	hello, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		t.Fatalf("body type = %T, want HelloBody", frame.Body)
	}
	if hello.Nonce != 0 || hello.Caps != protocol.CapTCPFallback || hello.FECProfile != protocol.FECProfileOff {
		t.Fatalf("fallback HELLO body = %+v", hello)
	}
}

func TestSendProbeTimeoutDoesNotRestartFallbackDialInFlight(t *testing.T) {
	streamTransport := &fakeStreamTransport{
		dialLeg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp0",
		},
	}
	in := New()
	in.streamTransport = streamTransport
	mustSendState(t, in, 99)
	in.negotiatedCaps.Store(uint32(protocol.CapTCPFallback))
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(udpLeg)
	lane.tcpRemote = "127.0.0.1:4321"
	lane.fallbackDialing = true
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	target := probe.Target(1)
	in.probeTargets = map[probe.Target]probeBinding{
		target: {sessionID: 99, laneID: 3, leg: udpLeg},
	}

	if err := in.handleProbeEvent(context.Background(), probe.Event{Type: probe.EventTargetLost, Target: target}); err != nil {
		t.Fatalf("handleProbeEvent failed: %v", err)
	}
	if lane.udpReady {
		t.Fatal("udpReady = true, want false after timeout")
	}
	if !lane.fallbackDialing {
		t.Fatal("fallbackDialing = false; in-flight fallback dial was cleared")
	}
	if len(streamTransport.dialed) != 0 {
		t.Fatalf("fallback dials = %d, want 0 while previous dial is in flight", len(streamTransport.dialed))
	}
}

func TestSendLegFailureMarksTCPNotReadyAndStartsFallback(t *testing.T) {
	streamTransport := &fakeStreamTransport{
		dialLeg: transport.LegRef{
			Kind:   transport.KindTCP,
			ConnID: "tcp-new",
		},
	}
	in := New()
	in.streamTransport = streamTransport
	in.probeEvents = make(chan probe.Event, 4)
	mustSendState(t, in, 99)
	in.negotiatedCaps.Store(uint32(protocol.CapTCPFallback))
	oldLeg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-old"}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(oldLeg)
	lane.tcpRemote = "127.0.0.1:4321"
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	in.trackProbeTarget(context.Background(), 99, 3, oldLeg)
	readProbeEvent(t, in.probeEvents)

	in.OnLegFailure(context.Background(), oldLeg, transport.ErrUnknownConn)
	waitForDialCount(t, streamTransport, 1)
	written := readSendPayload(t, in)
	written.Packet.Release()

	if lane.tcpReady {
		t.Fatal("tcpReady = true after TCP leg failure")
	}
	if !lane.fallbackDialing {
		t.Fatal("fallbackDialing = false after fallback dial result")
	}
	if got := streamTransport.closed["tcp-old"]; got != 1 {
		t.Fatalf("closed tcp-old count = %d, want 1", got)
	}
	if got := streamTransport.dialed[0]; got != "127.0.0.1:4321" {
		t.Fatalf("dialed remote = %q, want 127.0.0.1:4321", got)
	}
}

func TestSendProbeTargetLostIgnoresStaleLeg(t *testing.T) {
	streamTransport := &fakeStreamTransport{
		dialLeg: transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-next"},
	}
	in := New()
	in.streamTransport = streamTransport
	in.probeEvents = make(chan probe.Event, 8)
	mustSendState(t, in, 99)
	in.negotiatedCaps.Store(uint32(protocol.CapTCPFallback))

	oldLeg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-old"}
	newLeg := transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-new"}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(newLeg)
	lane.tcpRemote = "127.0.0.1:4321"
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	in.trackProbeTarget(context.Background(), 99, 3, oldLeg)
	readProbeEvent(t, in.probeEvents)
	target := in.probeKeys[newPingKey(oldLeg)]

	if err := in.handleProbeEvent(context.Background(), probe.Event{Type: probe.EventTargetLost, Target: target}); err != nil {
		t.Fatalf("handleProbeEvent failed: %v", err)
	}
	if !lane.tcpReady || lane.tcpLeg.ConnID != "tcp-new" {
		t.Fatalf("tcp leg = ready %t leg %+v, want tcp-new ready", lane.tcpReady, lane.tcpLeg)
	}
	if lane.fallbackDialing {
		t.Fatal("fallbackDialing = true after stale target loss")
	}
	if len(streamTransport.dialed) != 0 {
		t.Fatalf("fallback dials = %d, want 0 for stale target loss", len(streamTransport.dialed))
	}
	if _, ok := in.probeKeys[newPingKey(oldLeg)]; ok {
		t.Fatal("stale TCP probe target still tracked")
	}
}

func TestSendHandleCLOSELane(t *testing.T) {
	streamTransport := &fakeStreamTransport{}
	in := New()
	in.streamTransport = streamTransport
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp0"})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.CloseBody{Scope: protocol.CloseScopeLane, Reason: 1},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if err := writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("recv Write CLOSE lane failed: %v", err)
	}
	if _, ok := in.lanes[laneKey{sessionID: 99, laneID: 3}]; ok {
		t.Fatal("lane still exists after CLOSE lane")
	}
	if getSendState(in, 99) == nil {
		t.Fatal("session was removed by lane CLOSE")
	}
	if got := streamTransport.closed["tcp0"]; got != 1 {
		t.Fatalf("closed tcp0 count = %d, want 1", got)
	}
}

func TestRecvHandleREPAIRDoesNotTouchSendControlState(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	lane := newLaneRuntime(3, 1)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	out := newTestRecv(t, in)

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.RepairBody{BasePacketID: 100, Key: 7, SourceSpan: 4, Symbol: []byte("repair")},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if err := writeRecvPayload(context.Background(), out, testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload)); err != nil {
		t.Fatalf("recv Write REPAIR failed: %v", err)
	}

	if lane.udpReady {
		t.Fatal("lane udpReady = true, want false")
	}
	if lane.udpLeg.EndpointID != "" {
		t.Fatalf("lane endpoint = %q, want empty", lane.udpLeg.EndpointID)
	}
	assertNoSendPayload(t, in)
	assertNoRecvPacket(t, out)
}

func TestSendHandleREPAIRRecoversMissingPacket(t *testing.T) {
	tun := &recordTUNWriter{}
	out := newTestRecv(t, nil)
	recoveredPacket := ipv4TestPacket(20)

	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	hello, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeHELLO,
		SessionID: 99,
		LaneID:    3,
		Body: protocol.HelloBody{
			Nonce:      1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode HELLO: %v", err)
	}
	if err := writeRecvPayload(context.Background(), out, testEvent(leg, hello)); err != nil {
		t.Fatalf("recv HELLO failed: %v", err)
	}

	for _, item := range []struct {
		packetID uint32
		packet   []byte
	}{
		{packetID: 100, packet: []byte("a")},
		{packetID: 102, packet: []byte("c")},
		{packetID: 103, packet: []byte("d")},
	} {
		payload, err := protocol.Encode(protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 99,
			LaneID:    3,
			Body:      protocol.DataBody{PacketID: item.packetID, Packet: item.packet},
		}, nil)
		if err != nil {
			t.Fatalf("Encode DATA %d: %v", item.packetID, err)
		}
		if err := writeRecvPayload(context.Background(), out, testEvent(leg, payload)); err != nil {
			t.Fatalf("recv DATA %d failed: %v", item.packetID, err)
		}
	}

	codec, err := fecpkg.NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{[]byte("a"), recoveredPacket, []byte("c"), []byte("d"), nil}
	if err := codec.Encode(shards, 7); err != nil {
		t.Fatalf("Encode repair: %v", err)
	}
	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.RepairBody{BasePacketID: 100, Key: 7, SourceSpan: 4, Symbol: shards[4]},
	}, nil)
	if err != nil {
		t.Fatalf("Encode REPAIR: %v", err)
	}
	if err := writeRecvPayload(context.Background(), out, testEvent(leg, payload)); err != nil {
		t.Fatalf("recv Write REPAIR failed: %v", err)
	}
	drainRecvPacketsToTUN(t, out, tun)
	if tun.calls != 4 || string(tun.packet) != string(recoveredPacket) {
		t.Fatalf("tun writes = %d last packet %v, want recovered packet on fourth write", tun.calls, tun.packet)
	}

	lateDATA, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 101, Packet: recoveredPacket},
	}, nil)
	if err != nil {
		t.Fatalf("Encode DATA: %v", err)
	}
	if err := writeRecvPayload(context.Background(), out, testEvent(leg, lateDATA)); err != nil {
		t.Fatalf("recv Write late DATA failed: %v", err)
	}
	drainRecvPacketsToTUN(t, out, tun)
	if tun.calls != 4 {
		t.Fatalf("tun writes after late DATA = %d, want 4", tun.calls)
	}
}

func TestSendHandleCLOSESession(t *testing.T) {
	streamTransport := &fakeStreamTransport{}
	in := New()
	in.streamTransport = streamTransport
	mustSendState(t, in, 99)
	in.activateSession(99)
	lane1 := newLaneRuntime(1, 1)
	lane1.bindLeg(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp1"})
	lane2 := newLaneRuntime(2, 1)
	lane2.bindLeg(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp2"})
	otherLane := newLaneRuntime(1, 1)
	otherLane.bindLeg(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp-other"})
	in.lanes[laneKey{sessionID: 99, laneID: 1}] = lane1
	in.lanes[laneKey{sessionID: 99, laneID: 2}] = lane2
	in.lanes[laneKey{sessionID: 100, laneID: 1}] = otherLane
	in.strategy(99)

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeCLOSE,
		SessionID: 99,
		LaneID:    protocol.SessionControlLaneID,
		Body:      protocol.CloseBody{Scope: protocol.CloseScopeSession, Reason: 1},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if err := writeTestControl(context.Background(), in, testEvent(transport.LegRef{}, payload)); err != nil {
		t.Fatalf("recv Write CLOSE session failed: %v", err)
	}
	if getSendState(in, 99) != nil {
		t.Fatal("session still exists after CLOSE session")
	}
	if in.strategies[99] != nil {
		t.Fatal("strategy still exists after CLOSE session")
	}
	if in.hasActiveSession.Load() || in.activeSessionID.Load() != 0 {
		t.Fatalf("active session = (%v,%d), want cleared", in.hasActiveSession.Load(), in.activeSessionID.Load())
	}
	if err := in.handleTUNPacket(context.Background(), []byte("ip-packet")); err != nil {
		t.Fatalf("handleTUNPacket after session CLOSE failed: %v", err)
	}
	if getSendState(in, 99) != nil {
		t.Fatal("closed active session was recreated by TUN packet")
	}
	if _, ok := in.lanes[laneKey{sessionID: 99, laneID: 1}]; ok {
		t.Fatal("lane 1 still exists after CLOSE session")
	}
	if _, ok := in.lanes[laneKey{sessionID: 99, laneID: 2}]; ok {
		t.Fatal("lane 2 still exists after CLOSE session")
	}
	if _, ok := in.lanes[laneKey{sessionID: 100, laneID: 1}]; !ok {
		t.Fatal("other session lane was removed")
	}
	if got := streamTransport.closed["tcp1"]; got != 1 {
		t.Fatalf("closed tcp1 count = %d, want 1", got)
	}
	if got := streamTransport.closed["tcp2"]; got != 1 {
		t.Fatalf("closed tcp2 count = %d, want 1", got)
	}
	if got := streamTransport.closed["tcp-other"]; got != 0 {
		t.Fatalf("closed tcp-other count = %d, want 0", got)
	}
}

func TestPacketTransportWritesRecv(t *testing.T) {
	tun := &recordTUNWriter{wrote: make(chan struct{}, 1)}
	fake := &fakePacketTransport{sent: make(chan struct{})}

	payload, err := protocol.Encode(protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    3,
		Body:      protocol.DataBody{PacketID: 1, Packet: []byte("ip-packet")},
	}, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	fake.event = testEvent(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}, payload)

	ctx, cancel := context.WithCancel(context.Background())
	out := newTestRecv(t, nil)
	errCh := make(chan error, 2)
	go func() {
		errCh <- fake.Run(ctx, out)
	}()
	go func() {
		errCh <- tunio.RunWriter(ctx, out.Packets(), tun)
	}()

	<-fake.sent
	select {
	case <-tun.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TUN write")
	}
	cancel()
	for i := 0; i < 2; i++ {
		if err := <-errCh; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	}
	if string(tun.packet) != "ip-packet" {
		t.Fatalf("tun packet = %q, want ip-packet", tun.packet)
	}
}

func TestTUNRunWritesSend(t *testing.T) {
	reader := &fakeTUNReader{
		packets: [][]byte{[]byte("ip-packet")},
		sent:    make(chan struct{}),
	}
	in := New()
	in.activateSession(99)
	lane := newLaneRuntime(3, 1)
	lane.observeLeg(transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	})
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- tunio.Run(ctx, reader, testSendPacketWriter{send: in})
	}()

	<-reader.sent
	written := readSendPayload(t, in)
	defer written.Packet.Release()
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	frame, err := protocol.Decode(written.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode written payload: %v", err)
	}
	if frame.Type != protocol.TypeDATA {
		t.Fatalf("frame type = %d, want DATA", frame.Type)
	}
	if frame.SessionID != 99 || frame.LaneID != 3 {
		t.Fatalf("frame route = session %d lane %d, want session 99 lane 3", frame.SessionID, frame.LaneID)
	}
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		t.Fatalf("body type = %T, want DataBody", frame.Body)
	}
	if body.PacketID != 0 || string(body.Packet) != "ip-packet" {
		t.Fatalf("DATA = (%d,%q), want (0,ip-packet)", body.PacketID, body.Packet)
	}
}

type recordTUNWriter struct {
	calls  int
	packet []byte
	wrote  chan struct{}
}

type fakeTUNReader struct {
	packets [][]byte
	sent    chan struct{}
}

type testSendPacketWriter struct {
	send *Send
}

type fakePacketTransport struct {
	event transport.Payload
	sent  chan struct{}
}

func (r *fakeTUNReader) ReadPacket(ctx context.Context) (*packetbuf.Packet, error) {
	if len(r.packets) > 0 {
		packet := packetbuf.Acquire(len(r.packets[0]))
		copy(packet.Payload, r.packets[0])
		r.packets = r.packets[1:]
		close(r.sent)
		return packet, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (w testSendPacketWriter) Write(ctx context.Context, packet *packetbuf.Packet) error {
	return w.send.Write(ctx, packet)
}

type fakeStreamTransport struct {
	closed  map[string]int
	dialed  []string
	dialLeg transport.LegRef
	dialErr error
}

type recordPacketTransport struct {
	recordPacketWriter
}

type fakeFECCodec struct {
	encodeFunc      func(shards [][]byte, key uint16) error
	reconstructFunc func(shards [][]byte, key uint16) error
}

func (f *fakePacketTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	if err := writer.WriteTo(ctx, f.event.Leg, f.event.Packet); err != nil {
		return err
	}
	close(f.sent)
	select {
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakePacketTransport) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	return len(payload), nil
}

func (r *recordPacketTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeStreamTransport) Run(ctx context.Context, writer transport.PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeStreamTransport) Dial(ctx context.Context, remote string) (transport.LegRef, error) {
	f.dialed = append(f.dialed, remote)
	return f.dialLeg, f.dialErr
}

func (f *fakeStreamTransport) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	return len(payload), nil
}

func (f *fakeStreamTransport) Close(ctx context.Context, connID string) error {
	if f.closed == nil {
		f.closed = make(map[string]int)
	}
	f.closed[connID]++
	return nil
}

func (f *fakeFECCodec) Encode(shards [][]byte, key uint16) error {
	if f.encodeFunc == nil {
		return nil
	}
	return f.encodeFunc(shards, key)
}

func (f *fakeFECCodec) Reconstruct(shards [][]byte, key uint16) error {
	if f.reconstructFunc == nil {
		return nil
	}
	return f.reconstructFunc(shards, key)
}

func testEvent(leg transport.LegRef, payload []byte) transport.Payload {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	return transport.Payload{
		Leg:    leg,
		Packet: packet,
	}
}

func writeTestControl(ctx context.Context, in *Send, event transport.Payload) error {
	defer event.Packet.Release()
	frame, err := protocol.Decode(event.Packet.Payload)
	if err != nil {
		return err
	}
	state := NewRecvState(in)
	switch frame.Type {
	case protocol.TypeHELLO:
		return state.OnHello(ctx, event.Leg, frame)
	case protocol.TypeHELLOACK:
		return state.OnHelloAck(ctx, event.Leg, frame)
	case protocol.TypePING:
		return state.OnPing(ctx, event.Leg, frame)
	case protocol.TypePONG:
		return state.OnPong(ctx, event.Leg, frame)
	case protocol.TypeCLOSE:
		return state.OnClose(ctx, event.Leg, frame)
	default:
		return nil
	}
}

func writeRecvPayload(ctx context.Context, out *recvpkg.Recv, event transport.Payload) error {
	return out.WriteTo(ctx, event.Leg, event.Packet)
}

func getSendState(in *Send, sessionID uint64) *sendState {
	_, state, ok := in.getSessionState(sessionID)
	if !ok {
		return nil
	}
	return state
}

func mustSendState(t *testing.T, in *Send, sessionID uint64) *sendState {
	t.Helper()
	_, state, ok := in.getOrCreateSessionState(sessionID)
	if !ok {
		t.Fatalf("missing session %d", sessionID)
	}
	return state
}

func mustSession(t *testing.T, in *Send, sessionID uint64) *sessionpkg.Session {
	t.Helper()
	sessionState, ok := in.sessionManager.GetOrCreate(sessionID)
	if !ok {
		t.Fatalf("missing session %d", sessionID)
	}
	return sessionState
}

func startTestHELLORoute(t *testing.T, in *Send, sessionID uint64, laneID uint8) uint64 {
	t.Helper()
	sessionState, ok := in.sessionManager.Get(sessionID)
	if !ok {
		t.Fatalf("missing session %d", sessionID)
	}
	hello := sessionState.Open(0)
	var nonce uint64
	if err := hello.Do(func(v sessionpkg.View) error {
		nonce = v.Nonce()
		return nil
	}); err != nil {
		t.Fatalf("Hello.Do: %v", err)
	}
	var route helloRoute
	route.set(hello, transport.LegRef{}, []byte("hello"), in.probeTimeout)
	in.helloRoutes[laneKey{sessionID: sessionID, laneID: laneID}] = route
	return nonce
}

func waitForDialCount(t *testing.T, stream *fakeStreamTransport, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(stream.dialed) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dial count = %d, want at least %d", len(stream.dialed), want)
}

func readSendPayload(t *testing.T, in *Send) transport.Payload {
	t.Helper()
	select {
	case payload := <-in.Packets():
		return payload
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send payload")
		return transport.Payload{}
	}
}

func readProbeEvent(t *testing.T, events <-chan probe.Event) probe.Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for probe event")
		return probe.Event{}
	}
}

func assertNoSendPayload(t *testing.T, in *Send) {
	t.Helper()
	select {
	case payload := <-in.Packets():
		payload.Packet.Release()
		t.Fatal("unexpected send payload")
	default:
	}
}

func newTestRecv(t *testing.T, in *Send) *recvpkg.Recv {
	t.Helper()
	if in == nil {
		manager := &sessionpkg.Manager{}
		if _, ok := manager.Create(99); !ok {
			t.Fatal("Create session failed")
		}
		return recvpkg.New(recvpkg.Config{SessionManager: manager})
	}
	return recvpkg.New(recvpkg.Config{Control: NewRecvState(in), SessionManager: in.sessionManager})
}

type testProbeLoopRunner struct {
	send     *Send
	events   <-chan probe.Event
	interval time.Duration
	timeout  time.Duration
}

func (w testProbeLoopRunner) Run(ctx context.Context) error {
	if w.interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	runnerOut := make(chan probe.Event, 128)
	runnerErr := make(chan error, 1)
	if w.events != nil {
		runner := probe.New(probe.Config{
			Interval:       w.interval,
			Timeout:        w.timeout,
			MaxLoss:        1,
			RecoverSuccess: 1,
		})
		go func() {
			runnerErr <- runner.Run(ctx, w.events, runnerOut)
		}()
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-runnerErr:
			return err
		case event := <-runnerOut:
			if err := w.send.handleProbeEvent(ctx, event); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := w.send.retryOpenHELLO(ctx, uint64(now.UnixMilli())); err != nil {
				return err
			}
		}
	}
}

func writeRecvPacketToTUN(t *testing.T, out *recvpkg.Recv, tun *recordTUNWriter) {
	t.Helper()
	select {
	case packet := <-out.Packets():
		_, err := tun.WritePacket(context.Background(), packet.Payload)
		packet.Release()
		if err != nil {
			t.Fatalf("TUN write failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recv packet")
	}
}

func waitStreamWrite(t *testing.T, stream *recordStreamWriter) {
	t.Helper()
	if stream.wrote == nil {
		return
	}
	select {
	case <-stream.wrote:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream write")
	}
}

func drainRecvPacketsToTUN(t *testing.T, out *recvpkg.Recv, tun *recordTUNWriter) {
	t.Helper()
	for {
		select {
		case packet := <-out.Packets():
			_, err := tun.WritePacket(context.Background(), packet.Payload)
			packet.Release()
			if err != nil {
				t.Fatalf("TUN write failed: %v", err)
			}
		default:
			return
		}
	}
}

func assertNoRecvPacket(t *testing.T, out *recvpkg.Recv) {
	t.Helper()
	select {
	case packet := <-out.Packets():
		packet.Release()
		t.Fatal("recv emitted packet")
	default:
	}
}

func ipv4TestPacket(totalLen int) []byte {
	packet := make([]byte, totalLen)
	packet[0] = 0x45
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	for i := 20; i < totalLen; i++ {
		packet[i] = byte(i)
	}
	return packet
}

func (w *recordTUNWriter) WritePacket(ctx context.Context, packet []byte) (int, error) {
	w.calls++
	w.packet = append([]byte(nil), packet...)
	if w.wrote != nil {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return len(packet), nil
}
