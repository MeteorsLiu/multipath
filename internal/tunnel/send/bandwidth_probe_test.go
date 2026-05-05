package send

import (
	"context"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

func TestSendReceiveBandwidthProbeRepliesWithBitmap(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)
	leg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(leg)
	in.lanes[laneKey{sessionID: 99, laneID: 3}] = lane

	if err := in.receiveBandwidthProbe(context.Background(), 99, 3, leg, protocol.BandwidthProbeBody{
		ProbeID: 7,
		Seq:     0,
		Count:   2,
		SendMS:  1000,
		Payload: []byte("probe"),
	}); err != nil {
		t.Fatalf("receiveBandwidthProbe seq0 failed: %v", err)
	}
	first := readSendPayload(t, in)
	defer first.Packet.Release()
	firstFrame, err := protocol.Decode(first.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode first ack: %v", err)
	}
	firstAck, ok := firstFrame.Body.(protocol.BandwidthProbeAckBody)
	if !ok || firstFrame.Type != protocol.TypeBandwidthProbeAck {
		t.Fatalf("first frame = type %d body %T, want BW_PROBE_ACK", firstFrame.Type, firstFrame.Body)
	}
	if firstAck.ProbeID != 7 || firstAck.Count != 2 || firstAck.Received != 0x01 {
		t.Fatalf("first ack = %+v, want probe 7 count 2 received 0x01", firstAck)
	}

	if err := in.receiveBandwidthProbe(context.Background(), 99, 3, leg, protocol.BandwidthProbeBody{
		ProbeID: 7,
		Seq:     1,
		Count:   2,
		SendMS:  1001,
		Payload: []byte("probe"),
	}); err != nil {
		t.Fatalf("receiveBandwidthProbe seq1 failed: %v", err)
	}
	second := readSendPayload(t, in)
	defer second.Packet.Release()
	secondFrame, err := protocol.Decode(second.Packet.Payload)
	if err != nil {
		t.Fatalf("Decode second ack: %v", err)
	}
	secondAck := secondFrame.Body.(protocol.BandwidthProbeAckBody)
	if secondAck.Received != 0x03 {
		t.Fatalf("second received = %#x, want 0x03", secondAck.Received)
	}
}

func TestSendBandwidthProbeAckUpdatesLaneQuality(t *testing.T) {
	in := New()
	mustSendState(t, in, 99)
	in.activateSession(99)
	key := laneKey{sessionID: 99, laneID: 3}
	udpLeg := transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(udpLeg)
	in.lanes[key] = lane

	legKey := newPingKey(udpLeg)
	in.bandwidthLegs[legKey] = &bandwidthLegState{
		nextRateBps: bandwidthProbeMinRateBps,
		inFlight:    true,
	}
	in.bandwidthPending[7] = &bandwidthProbeRound{
		key:          key,
		leg:          udpLeg,
		legKey:       legKey,
		probeID:      7,
		count:        4,
		payloadBytes: 1000,
	}

	if err := in.receiveBandwidthProbeAck(99, 3, udpLeg, protocol.BandwidthProbeAckBody{
		ProbeID:   7,
		Count:     4,
		Received:  0x03,
		FirstRXMS: 1000,
		LastRXMS:  1001,
	}); err != nil {
		t.Fatalf("receiveBandwidthProbeAck failed: %v", err)
	}
	in.finishBandwidthProbe(7)

	_, udpQ, _, _ := lane.legQualities()
	if udpQ.BandwidthBps == 0 {
		t.Fatal("udp bandwidth EWMA was not updated")
	}
	if udpQ.ProbeLoss != 0.5 {
		t.Fatalf("udp probe loss = %.2f, want 0.50", udpQ.ProbeLoss)
	}
}

func TestLaneBandwidthQoSHysteresis(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	for i := 0; i < minBandwidthProbeSamples+bandwidthProbeBadSamplesToSwitch; i++ {
		lane.recordBandwidthSample(transport.KindTCP, 120_000_000, 0)
		lane.recordBandwidthSample(transport.KindUDP, 20_000_000, 0.50)
	}
	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after consecutive bad samples")
	}

	lane.recordBandwidthSample(transport.KindUDP, 120_000_000, 0)
	_, udpQ, _, _ = lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS cleared after only one good sample")
	}
	lane.recordBandwidthSample(transport.KindUDP, 120_000_000, 0)
	_, udpQ, _, _ = lane.legQualities()
	if udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS did not clear after consecutive good samples")
	}
}
