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
	if state := in.bandwidthLegs[legKey]; state == nil || !state.complete {
		t.Fatalf("probe complete = %v, want true after loss", state != nil && state.complete)
	}
}

func TestLaneBandwidthQoSClassification(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	lane.recordBandwidthSample(transport.KindUDP, 59_000_000, 0.041)
	lane.recordBandwidthSample(transport.KindTCP, 81_000_000, 0)

	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after UDP probe loss")
	}
	if !udpQ.BandwidthPreferTCP {
		t.Fatal("TCP was not preferred when UDP had loss and TCP bandwidth was higher")
	}
}

func TestLaneBandwidthQoSLimitedButKeepsUDPWhenTCPIsSlower(t *testing.T) {
	lane := newLaneRuntime(3, 10)
	lane.recordBandwidthSample(transport.KindUDP, 115_000_000, 0.021)
	lane.recordBandwidthSample(transport.KindTCP, 35_000_000, 0)

	_, udpQ, _, _ := lane.legQualities()
	if !udpQ.BandwidthQoSLimited {
		t.Fatal("UDP bandwidth QoS was not marked after UDP probe loss")
	}
	if udpQ.BandwidthPreferTCP {
		t.Fatal("TCP was preferred even though TCP bandwidth was lower than UDP")
	}
}

func TestBandwidthSamplePlateau(t *testing.T) {
	if samplePlateau(0, 1) {
		t.Fatal("zero best sample should not be plateau")
	}
	if !samplePlateau(1000, 1100) {
		t.Fatal("10% sample gain should be plateau")
	}
	if samplePlateau(1000, 1101) {
		t.Fatal("sample gain above 10% should continue probing")
	}
}

func TestTCPBandwidthProbeDoesNotCompleteBeforeMinimumRounds(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	tcpLeg := transport.LegRef{
		Kind:   transport.KindTCP,
		ConnID: "tcp-1",
	}
	lane := newLaneRuntime(3, 10)
	lane.bindLeg(tcpLeg)
	in.lanes[key] = lane

	legKey := newPingKey(tcpLeg)
	state := &bandwidthLegState{nextRateBps: bandwidthProbeMinRateBps}
	in.bandwidthLegs[legKey] = state

	for i := 0; i < bandwidthProbeTCPMinRounds-1; i++ {
		probeID := uint64(i + 1)
		state.inFlight = true
		in.bandwidthPending[probeID] = &bandwidthProbeRound{
			key:          key,
			leg:          tcpLeg,
			legKey:       legKey,
			probeID:      probeID,
			count:        4,
			payloadBytes: 1000,
			received:     0x0f,
			firstRXMS:    1000,
			lastRXMS:     1032,
		}
		in.finishBandwidthProbe(probeID)
		if state.complete {
			t.Fatalf("tcp probe completed after %d rounds, want at least %d", i+1, bandwidthProbeTCPMinRounds)
		}
	}

	probeID := uint64(bandwidthProbeTCPMinRounds)
	state.inFlight = true
	in.bandwidthPending[probeID] = &bandwidthProbeRound{
		key:          key,
		leg:          tcpLeg,
		legKey:       legKey,
		probeID:      probeID,
		count:        4,
		payloadBytes: 1000,
		received:     0x0f,
		firstRXMS:    1000,
		lastRXMS:     1032,
	}
	in.finishBandwidthProbe(probeID)
	if !state.complete {
		t.Fatalf("tcp probe did not complete after %d plateau rounds", bandwidthProbeTCPMinRounds)
	}
}
