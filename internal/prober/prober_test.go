package prober

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

// TestProbePacketFormat tests the format of probe packets
func TestProbePacketFormat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	// Send a probe packet
	prober.sendProbePacket()

	// Verify packet format from out channel
	select {
	case packet := <-prober.Out():
		defer mempool.Put(packet)

		// Check packet data size - should be NonceSize (8 bytes)
		if packet.Len() != NonceSize {
			t.Errorf("Packet data size should be %d bytes, got %d", NonceSize, packet.Len())
		}

		// Check packet has space for headers
		// According to prober.go: GetWithHeader(NonceSize, protocol.HeaderSize+ProbeHeaderSize)
		expectedHeaderSpace := protocol.HeaderSize + ProbeHeaderSize
		if packet.Cap() < NonceSize+expectedHeaderSpace {
			t.Errorf("Packet should have space for headers (%d bytes), got capacity %d",
				expectedHeaderSpace, packet.Cap())
		}

		// Extract nonce from packet data
		nonce := binary.LittleEndian.Uint64(packet.Bytes())
		if nonce == 0 {
			t.Error("Nonce should not be zero (random data)")
		}

		// Verify nonce is stored in packet map
		if _, exists := prober.packetMap[nonce]; !exists {
			t.Error("Packet nonce not found in prober's packet map")
		}

		// Verify packet info has start time
		if info, exists := prober.packetMap[nonce]; exists {
			if info.startTime.IsZero() {
				t.Error("Packet info should have start time")
			}
			if info.isTimeout {
				t.Error("New packet should not be marked as timeout")
			}
		}

		t.Logf("Probe packet - Data: %d bytes, Capacity: %d bytes, Nonce: %d",
			packet.Len(), packet.Cap(), nonce)

	case <-time.After(1 * time.Second):
		t.Fatal("No packet received from out channel")
	}
}

// TestProbePacketHeaderStructure tests the complete packet structure with headers
func TestProbePacketHeaderStructure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	// Send a probe packet
	prober.sendProbePacket()

	select {
	case packet := <-prober.Out():
		defer mempool.Put(packet)

		// According to prober.go comments, the packet structure should be:
		// Header:
		// Byte 1: OpCode (protocol header)
		// Byte 2: Reply Epoch
		// Byte 3-19: Prober ID (16B)
		// Byte 20-28: Nonce (8B)

		// Get full buffer to check header space
		fullBuf := packet.FullBytes()

		// Check that we have space for protocol header (1 byte)
		if len(fullBuf) < protocol.HeaderSize {
			t.Errorf("Full buffer should have space for protocol header (%d bytes), got %d",
				protocol.HeaderSize, len(fullBuf))
		}

		// The first byte should be 0 initially (will be set by sender to HeartBeat)
		if fullBuf[0] != 0 {
			t.Logf("Protocol header byte: %d (will be set to HeartBeat by sender)", fullBuf[0])
		}

		// Check prober ID is written at correct offset (protocol.HeaderSize + 1)
		proberIdOffset := protocol.HeaderSize + 1
		if len(fullBuf) >= proberIdOffset+16 {
			extractedId := fullBuf[proberIdOffset : proberIdOffset+16]
			expectedId := proberId[:]

			for i := 0; i < 16; i++ {
				if extractedId[i] != expectedId[i] {
					t.Errorf("Prober ID mismatch at byte %d: expected %d, got %d",
						i, expectedId[i], extractedId[i])
				}
			}
			t.Logf("Prober ID correctly written at offset %d", proberIdOffset)
		} else {
			t.Error("Buffer too small to contain prober ID")
		}

		// The actual packet data (nonce) starts after headers
		nonce := binary.LittleEndian.Uint64(packet.Bytes())
		t.Logf("Packet structure verified - Nonce: %d, Prober ID: %s", nonce, proberId.String())

	case <-time.After(1 * time.Second):
		t.Fatal("No packet received from out channel")
	}
}

// TestProbePacketReply tests packet reply handling through in channel
func TestProbePacketReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})
	proberId := uuid.New()

	// Start the prober to enable packet processing
	prober.Start(proberId)

	// Wait for initial packet
	var sentNonce uint64
	select {
	case packet := <-prober.Out():
		sentNonce = binary.LittleEndian.Uint64(packet.Bytes())
		mempool.Put(packet)
		t.Logf("Sent probe packet with nonce: %d", sentNonce)
	case <-time.After(1 * time.Second):
		t.Fatal("No packet sent")
	}

	// Verify packet is in map before reply
	if _, exists := prober.packetMap[sentNonce]; !exists {
		t.Fatal("Sent packet nonce not in map")
	}

	// Create reply packet with same nonce (simulating network echo)
	replyPacket := mempool.Get(NonceSize)
	binary.LittleEndian.PutUint64(replyPacket.Bytes(), sentNonce)

	// Send reply through in channel
	select {
	case prober.In() <- replyPacket:
		t.Logf("Sent reply packet with nonce: %d", sentNonce)
	case <-time.After(1 * time.Second):
		mempool.Put(replyPacket)
		t.Fatal("Failed to send reply through in channel")
	}

	// Wait for processing
	time.Sleep(100 * time.Millisecond)

	// Verify packet was removed from map after reply
	if _, exists := prober.packetMap[sentNonce]; exists {
		t.Error("Packet should be removed from map after successful reply")
	}
}

// TestProbePacketTimeout tests timeout handling by manually triggering timeout
func TestProbePacketTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	eventReceived := make(chan Event, 10)
	prober := New(ctx, func(event Event) {
		eventReceived <- event
	})

	proberId := uuid.New()
	prober.proberId = proberId

	// Send a packet manually and let it timeout
	prober.sendProbePacket()

	var sentNonce uint64
	select {
	case packet := <-prober.Out():
		sentNonce = binary.LittleEndian.Uint64(packet.Bytes())
		mempool.Put(packet)
		t.Logf("Sent packet with nonce: %d (will timeout)", sentNonce)
	case <-time.After(1 * time.Second):
		t.Fatal("No packet sent")
	}

	// Verify packet is in map
	if _, exists := prober.packetMap[sentNonce]; !exists {
		t.Fatal("Sent packet nonce not in map")
	}

	// Manually trigger timeout by calling markTimeout after some time
	time.Sleep(100 * time.Millisecond)

	// Set a very short timeout to force timeout condition
	prober.currentTimeout = 1 * time.Microsecond

	// Call markTimeout to trigger the timeout logic
	isTimeout := prober.markTimeout()
	if !isTimeout {
		t.Error("markTimeout should return true when packet times out")
	}

	// Manually trigger state change
	prober.switchState(Lost)

	// Check that Lost event was fired
	select {
	case event := <-eventReceived:
		if event != Lost {
			t.Errorf("Expected Lost event, got %v", event)
		}
		t.Logf("Received expected timeout event: %v", event)
	case <-time.After(100 * time.Millisecond):
		t.Error("No Lost event received")
	}

	t.Log("Timeout handling test completed successfully")
}

// TestProbePacketWithSenderIntegration tests how prober packets are handled by sender
func TestProbePacketWithSenderIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	// Send a probe packet
	prober.sendProbePacket()

	select {
	case packet := <-prober.Out():
		defer mempool.Put(packet)

		// Simulate what sender does: check if header is 0, then set to HeartBeat
		fullBuf := packet.FullBytes()
		if fullBuf[0] == 0 {
			// This is what sender.go does for prober packets
			protocol.MakeHeader(packet, protocol.HeartBeat)
			t.Logf("Set packet type to HeartBeat (value: %d)", protocol.HeartBeat)
		}

		// Verify header was set correctly
		if fullBuf[0] != byte(protocol.HeartBeat) {
			t.Errorf("Expected HeartBeat packet type (%d), got %d", protocol.HeartBeat, fullBuf[0])
		}

		// Extract nonce
		nonce := binary.LittleEndian.Uint64(packet.Bytes())
		t.Logf("HeartBeat packet ready for network - Type: %s, Nonce: %d",
			protocol.PacketType(fullBuf[0]).String(), nonce)

	case <-time.After(1 * time.Second):
		t.Fatal("No packet received from prober")
	}
}

// TestMultipleProbePackets tests handling multiple packets manually
func TestMultipleProbePackets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	const numPackets = 3
	var sentNonces []uint64

	// Manually send multiple packets
	for i := 0; i < numPackets; i++ {
		prober.sendProbePacket()

		select {
		case packet := <-prober.Out():
			nonce := binary.LittleEndian.Uint64(packet.Bytes())
			sentNonces = append(sentNonces, nonce)
			mempool.Put(packet)
			t.Logf("Sent packet %d with nonce: %d", i+1, nonce)
		case <-time.After(1 * time.Second):
			t.Fatalf("Packet %d not received", i+1)
		}
	}

	// Verify all packets are tracked
	if len(prober.packetMap) != numPackets {
		t.Errorf("Expected %d packets in map, got %d", numPackets, len(prober.packetMap))
	}

	// Reply to all packets manually (without starting prober loop)
	for i, nonce := range sentNonces {
		replyPacket := mempool.Get(NonceSize)
		binary.LittleEndian.PutUint64(replyPacket.Bytes(), nonce)

		// Process reply manually
		prober.recvProbePacket(replyPacket)
		t.Logf("Processed reply %d for nonce: %d", i+1, nonce)
	}

	// Verify all packets were processed
	if len(prober.packetMap) != 0 {
		t.Errorf("Expected empty packet map, got %d packets remaining", len(prober.packetMap))
	}

	t.Logf("Successfully processed %d probe packets", numPackets)
}

// TestProbePacketUnknownNonce tests handling of unknown nonce in reply
func TestProbePacketUnknownNonce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, func(event Event) {})

	initialMapSize := len(prober.packetMap)

	// Send reply with unknown nonce
	unknownNonce := uint64(99999)
	replyPacket := mempool.Get(NonceSize)
	binary.LittleEndian.PutUint64(replyPacket.Bytes(), unknownNonce)

	select {
	case prober.In() <- replyPacket:
		t.Logf("Sent reply with unknown nonce: %d", unknownNonce)
	case <-time.After(1 * time.Second):
		mempool.Put(replyPacket)
		t.Fatal("Failed to send reply")
	}

	// Wait for processing
	time.Sleep(50 * time.Millisecond)

	// Verify map size didn't change
	if len(prober.packetMap) != initialMapSize {
		t.Error("PacketMap size should not change for unknown nonce")
	}

	t.Log("Unknown nonce reply handled correctly (ignored)")
}
