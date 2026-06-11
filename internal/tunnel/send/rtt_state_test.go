package send

import (
	"context"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func TestSendRTTRecordsPingAndAcceptsMatchedPONG(t *testing.T) {
	in := New(Config{ProbeTimeout: time.Second})
	target := probe.Target(1)
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 1)
	lane.bindLeg(leg)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	in.probeTargets[target] = probeBinding{sessionID: 99, laneID: 3, leg: leg}

	if err := in.handleProbeEvent(context.Background(), probe.Event{
		Type:   probe.EventSendPing,
		Target: target,
		PingID: 7,
		TimeMS: 1000,
	}); err != nil {
		t.Fatalf("handleProbeEvent: %v", err)
	}
	written := readSendPayload(t, in)
	written.Packet.Release()

	if pending := lane.quality.InflightLen(); pending != 1 {
		t.Fatalf("rtt pending = %d, want 1", pending)
	}

	obs, _, ok := in.acceptRTTPong(lane, 99, 3, leg, target, protocol.PingBody{PingID: 7, TimeMS: 1000}, 1040)
	if !ok {
		t.Fatal("acceptRTTPong returned false")
	}
	if obs.SampleMS != 40 || obs.SRTTMS != 40 || obs.RTTVarMS != 20 || obs.Samples != 1 {
		t.Fatalf("observation = %+v, want sample=40 srtt=40 rttvar=20 samples=1", obs)
	}
	if pending := lane.quality.InflightLen(); pending != 0 {
		t.Fatalf("rtt pending after PONG = %d, want 0", pending)
	}
}

func TestSendRTTRejectsMismatchedPONG(t *testing.T) {
	in := New(Config{ProbeTimeout: time.Second})
	target := probe.Target(1)
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 1)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane
	binding := probeBinding{sessionID: 99, laneID: 3, leg: leg}
	in.recordRTTPing(target, 7, 1000, binding)

	if _, _, ok := in.acceptRTTPong(lane, 99, 3, leg, target, protocol.PingBody{PingID: 7, TimeMS: 999}, 1040); ok {
		t.Fatal("acceptRTTPong accepted mismatched timestamp")
	}

	if lane.quality.UDP(false).SmoothedRTT > 0 {
		t.Fatal("lane RTT sample was recorded for mismatched PONG")
	}
	if pending := lane.quality.InflightLen(); pending != 1 {
		t.Fatalf("rtt pending after rejected PONG = %d, want 1", pending)
	}
}

func TestSendSessionMaxMinRTTUseActiveLegs(t *testing.T) {
	in := New()
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	tcpLeg := transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp0",
	}

	laneA := newLaneRuntime(1, 1)
	laneA.bindLeg(udpLeg)
	laneA.quality.OnPongSample(transport.KindUDP, 30)

	laneB := newLaneRuntime(2, 1)
	laneB.bindLeg(tcpLeg)
	laneB.quality.OnPongSample(transport.KindTCP, 60)

	in.lanes[laneKey{sessionID: 99, laneID: 1}] = laneA
	in.lanes[laneKey{sessionID: 99, laneID: 2}] = laneB

	maxRTT, ok := in.sessionMaxRTTMs(99)
	if !ok || maxRTT != 60 {
		t.Fatalf("sessionMaxRTTMs = (%d,%t), want (60,true)", maxRTT, ok)
	}
	minRTT, ok := in.sessionMinRTTMs(99)
	if !ok || minRTT != 30 {
		t.Fatalf("sessionMinRTTMs = (%d,%t), want (30,true)", minRTT, ok)
	}
}
