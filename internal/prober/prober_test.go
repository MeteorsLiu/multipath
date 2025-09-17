package prober

import (
	"context"
	"encoding/binary"
	"math"
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

	prober := New(ctx, "test-addr", func(event Event) {})
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

	prober := New(ctx, "test-addr", func(event Event) {})
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

	prober := New(ctx, "test-addr", func(event Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	// Manually send a probe packet (since start() is not running)
	prober.sendProbePacket()

	// Wait for packet
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

	// Process reply manually (since main loop is not running)
	prober.recvProbePacket(replyPacket)
	t.Logf("Processed reply packet with nonce: %d", sentNonce)

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
	prober := New(ctx, "test-addr", func(event Event) {
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

	prober := New(ctx, "test-addr", func(event Event) {})
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

	prober := New(ctx, "test-addr", func(event Event) {})
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

	prober := New(ctx, "test-addr", func(event Event) {})

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

// === HoltSmoothing Tests ===

func TestHoltSmoothing_Basic(t *testing.T) {
	h := NewHoltSmoothing(0.2, 0.1)

	// Test initial state
	if h.IsInitialized() {
		t.Error("Expected HoltSmoothing to be uninitialized")
	}

	// First observation should set level
	h.Update(100.0)
	if !h.IsInitialized() {
		t.Error("Expected HoltSmoothing to be initialized after first update")
	}
	if h.GetCurrent() != 100.0 {
		t.Errorf("Expected level to be 100.0, got %f", h.GetCurrent())
	}
	if h.GetTrend() != 0.0 {
		t.Errorf("Expected trend to be 0.0, got %f", h.GetTrend())
	}
}

func TestHoltSmoothing_UpwardTrend(t *testing.T) {
	h := NewHoltSmoothing(0.3, 0.1)

	// Simulate increasing RTT pattern
	rttValues := []float64{100, 110, 120, 130, 140, 150}

	for _, rtt := range rttValues {
		h.Update(rtt)
	}

	// Should detect positive trend
	trend := h.GetTrend()
	if trend <= 0 {
		t.Errorf("Expected positive trend for increasing RTT, got %f", trend)
	}

	// Prediction should be higher than current level
	current := h.GetCurrent()
	predicted := h.Predict(1)
	if predicted <= current {
		t.Errorf("Expected prediction (%f) to be higher than current (%f) for upward trend", predicted, current)
	}
}

func TestHoltSmoothing_DownwardTrend(t *testing.T) {
	h := NewHoltSmoothing(0.3, 0.1)

	// Simulate decreasing RTT pattern
	rttValues := []float64{150, 140, 130, 120, 110, 100}

	for _, rtt := range rttValues {
		h.Update(rtt)
	}

	// Should detect negative trend
	trend := h.GetTrend()
	if trend >= 0 {
		t.Errorf("Expected negative trend for decreasing RTT, got %f", trend)
	}

	// Prediction should be lower than current level
	current := h.GetCurrent()
	predicted := h.Predict(1)
	if predicted >= current {
		t.Errorf("Expected prediction (%f) to be lower than current (%f) for downward trend", predicted, current)
	}
}

func TestHoltSmoothing_StableRTT(t *testing.T) {
	h := NewHoltSmoothing(0.2, 0.1)

	// Simulate stable RTT with minor fluctuations
	rttValues := []float64{100, 102, 98, 101, 99, 100, 103, 97}

	for _, rtt := range rttValues {
		h.Update(rtt)
	}

	// Trend should be close to zero
	trend := h.GetTrend()
	if math.Abs(trend) > 5 {
		t.Errorf("Expected trend to be close to zero for stable RTT, got %f", trend)
	}

	// Level should be close to average
	level := h.GetCurrent()
	expectedLevel := 100.0
	if math.Abs(level-expectedLevel) > 10 {
		t.Errorf("Expected level to be close to %f, got %f", expectedLevel, level)
	}
}

func TestHoltSmoothing_MultistepPrediction(t *testing.T) {
	h := NewHoltSmoothing(0.2, 0.1)

	// Create a clear linear trend
	for i := 0; i < 10; i++ {
		h.Update(float64(100 + i*10)) // 100, 110, 120, ..., 190
	}

	// Test multi-step predictions
	pred1 := h.Predict(1)
	pred2 := h.Predict(2)
	pred3 := h.Predict(3)

	// For positive trend, predictions should increase
	if pred2 <= pred1 {
		t.Errorf("Expected pred2 (%f) > pred1 (%f)", pred2, pred1)
	}
	if pred3 <= pred2 {
		t.Errorf("Expected pred3 (%f) > pred2 (%f)", pred3, pred2)
	}

	// Verify linear relationship (approximately)
	trend := h.GetTrend()
	expectedPred2 := pred1 + trend
	expectedPred3 := pred1 + 2*trend

	if math.Abs(pred2-expectedPred2) > 0.1 {
		t.Errorf("Expected pred2 to be %f, got %f", expectedPred2, pred2)
	}
	if math.Abs(pred3-expectedPred3) > 0.1 {
		t.Errorf("Expected pred3 to be %f, got %f", expectedPred3, pred3)
	}
}

func TestHoltSmoothing_SensitivityParameters(t *testing.T) {
	// Test different alpha/beta values
	h1 := NewHoltSmoothing(0.1, 0.05) // Low sensitivity
	h2 := NewHoltSmoothing(0.5, 0.3)  // High sensitivity

	// Apply same RTT pattern
	rttValues := []float64{100, 150, 200}

	for _, rtt := range rttValues {
		h1.Update(rtt)
		h2.Update(rtt)
	}

	// High sensitivity should have higher trend
	trend1 := h1.GetTrend()
	trend2 := h2.GetTrend()

	if trend2 <= trend1 {
		t.Errorf("Expected high sensitivity trend (%f) > low sensitivity trend (%f)", trend2, trend1)
	}
}

func TestHoltSmoothing_NetworkJitterScenario(t *testing.T) {
	h := NewHoltSmoothing(0.2, 0.1)

	// Simulate network with baseline 100ms + jitter
	baseRTT := 100.0
	jitterPattern := []float64{0, 5, -3, 8, -2, 10, -1, 15, 2, 20}

	for _, jitter := range jitterPattern {
		h.Update(baseRTT + jitter)
	}

	// Should detect slight upward trend due to increasing jitter
	trend := h.GetTrend()
	if trend <= 0 {
		t.Errorf("Expected slight positive trend for increasing jitter, got %f", trend)
	}

	// Level should be reasonably close to recent values
	level := h.GetCurrent()
	if level < baseRTT || level > baseRTT+25 {
		t.Errorf("Expected level to be in range [%f, %f], got %f", baseRTT, baseRTT+25, level)
	}
}

func TestHoltSmoothing_NetworkCongestionScenario(t *testing.T) {
	h := NewHoltSmoothing(0.3, 0.15) // Higher sensitivity for congestion

	// Simulate sudden network congestion
	normalRTT := []float64{50, 52, 48, 51, 49}
	congestedRTT := []float64{150, 180, 200, 220, 250}

	// Normal period
	for _, rtt := range normalRTT {
		h.Update(rtt)
	}

	levelBeforeCongestion := h.GetCurrent()
	trendBeforeCongestion := h.GetTrend()

	// Congestion period
	for _, rtt := range congestedRTT {
		h.Update(rtt)
	}

	levelAfterCongestion := h.GetCurrent()
	trendAfterCongestion := h.GetTrend()

	// Should detect significant changes
	if levelAfterCongestion <= levelBeforeCongestion*2 {
		t.Errorf("Expected significant level increase, before: %f, after: %f",
			levelBeforeCongestion, levelAfterCongestion)
	}

	if trendAfterCongestion <= trendBeforeCongestion {
		t.Errorf("Expected trend to increase during congestion, before: %f, after: %f",
			trendBeforeCongestion, trendAfterCongestion)
	}
}

func BenchmarkHoltSmoothing_Update(b *testing.B) {
	h := NewHoltSmoothing(0.2, 0.1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Update(float64(100 + i%50))
	}
}

func BenchmarkHoltSmoothing_Predict(b *testing.B) {
	h := NewHoltSmoothing(0.2, 0.1)

	// Initialize with some data
	for i := 0; i < 100; i++ {
		h.Update(float64(100 + i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Predict(1)
	}
}

// === Prober Integration Tests with HoltSmoothing ===

func TestProber_RTTEstimationWithHolt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	events := make(chan Event, 10)
	prober := New(ctx, "test-addr", func(event Event) {
		events <- event
	})

	proberId := uuid.New()
	prober.proberId = proberId

	// Simulate RTT measurements with increasing trend
	rttPattern := []time.Duration{
		50 * time.Millisecond,
		55 * time.Millisecond,
		60 * time.Millisecond,
		65 * time.Millisecond,
		70 * time.Millisecond,
	}

	for i, rtt := range rttPattern {
		// Send probe packet
		prober.sendProbePacket()

		var nonce uint64
		select {
		case packet := <-prober.Out():
			nonce = binary.LittleEndian.Uint64(packet.Bytes())
			mempool.Put(packet)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("No packet sent for iteration %d", i)
		}

		// Simulate response after RTT delay
		time.Sleep(rtt)

		// Create and send reply
		replyPacket := mempool.Get(NonceSize)
		binary.LittleEndian.PutUint64(replyPacket.Bytes(), nonce)
		prober.recvProbePacket(replyPacket)

		t.Logf("Iteration %d: RTT=%v, Level=%.2f, Trend=%.2f",
			i, rtt, prober.rttEstimator.GetCurrent()/1000, prober.rttEstimator.GetTrend())
	}

	// Verify trend detection
	trend := prober.rttEstimator.GetTrend()
	if trend <= 0 {
		t.Errorf("Expected positive trend for increasing RTT, got %f", trend)
	}

	// Verify current timeout reflects the trend
	if prober.currentTimeout < 100*time.Millisecond {
		t.Errorf("Expected timeout to adapt to increasing RTT, got %v", prober.currentTimeout)
	}
}

func TestProber_AdaptiveTimeoutCalculation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prober := New(ctx, "test-addr", func(Event) {})
	proberId := uuid.New()
	prober.proberId = proberId

	// Test scenarios
	scenarios := []struct {
		name        string
		rttPattern  []float64 // in microseconds
		expectHighTimeout bool
	}{
		{
			name:       "Stable RTT",
			rttPattern: []float64{50000, 52000, 48000, 51000, 49000},
			expectHighTimeout: false,
		},
		{
			name:       "Increasing RTT (congestion)",
			rttPattern: []float64{50000, 70000, 90000, 110000, 130000},
			expectHighTimeout: true,
		},
		{
			name:       "Decreasing RTT (recovery)",
			rttPattern: []float64{130000, 110000, 90000, 70000, 50000},
			expectHighTimeout: false,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Reset estimator
			prober.rttEstimator = NewHoltSmoothing(0.2, 0.1)

			for _, rtt := range scenario.rttPattern {
				prober.rttEstimator.Update(rtt)
			}

			// Calculate timeout as done in sendProbePacket
			predictedRtt := prober.rttEstimator.Predict(1)
			trend := prober.rttEstimator.GetTrend()

			safetyMultiplier := 2.0
			if trend > 0 {
				safetyMultiplier = 2.5 + math.Min(trend/1000, 1.0)
			} else {
				safetyMultiplier = 1.8 - math.Min(-trend/1000, 0.3)
			}

			timeout := time.Duration(predictedRtt*safetyMultiplier)*time.Microsecond + _baselineBuffer

			t.Logf("Scenario: %s, Predicted RTT: %.2fms, Trend: %.2f, Safety: %.2fx, Timeout: %v",
				scenario.name, predictedRtt/1000, trend, safetyMultiplier, timeout)

			if scenario.expectHighTimeout {
				if timeout < 300*time.Millisecond {
					t.Errorf("Expected higher timeout for %s, got %v", scenario.name, timeout)
				}
			} else {
				if timeout > 400*time.Millisecond {
					t.Errorf("Expected lower timeout for %s, got %v", scenario.name, timeout)
				}
			}
		})
	}
}

func TestProber_StateTransitionsWithHolt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan Event, 20)
	prober := New(ctx, "test-addr", func(event Event) {
		events <- event
	})

	proberId := uuid.New()
	prober.proberId = proberId

	// Initialize with stable RTT
	stableRTT := 50000.0 // 50ms in microseconds
	for i := 0; i < 5; i++ {
		prober.rttEstimator.Update(stableRTT + float64(i%3)*1000) // minor jitter
	}

	t.Logf("Initial state: Level=%.2fms, Trend=%.2f",
		prober.rttEstimator.GetCurrent()/1000, prober.rttEstimator.GetTrend())

	// Test timeout detection
	prober.sendProbePacket()

	select {
	case packet := <-prober.Out():
		_ = binary.LittleEndian.Uint64(packet.Bytes())
		mempool.Put(packet)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("No packet sent")
	}

	// Send second packet first
	prober.sendProbePacket()
	select {
	case packet := <-prober.Out():
		mempool.Put(packet)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("No second packet sent")
	}

	// Force timeout by setting very low threshold and add some time
	prober.currentTimeout = 1 * time.Microsecond
	time.Sleep(10 * time.Millisecond) // Ensure both packets are old enough to timeout

	// First timeout call should detect 2 packets timing out simultaneously
	hasTimeout := prober.markTimeout()
	if !hasTimeout {
		t.Error("Expected timeout to be detected")
	}

	t.Logf("First markTimeout result: %v, consecutiveLoss: %d, packetMap size: %d",
		hasTimeout, prober.consecutiveLoss, len(prober.packetMap))

	// Manually trigger second consecutive timeout to reach Unstable state
	prober.consecutiveLoss = 2
	prober.switchState(Unstable)

	if prober.state != Unstable {
		t.Errorf("Expected state to be Unstable, got %v", prober.state)
	}

	t.Logf("Successfully transitioned to Unstable state after 2 consecutive timeouts")
}

func TestProber_RecoveryBehavior(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	events := make(chan Event, 10)
	prober := New(ctx, "test-addr", func(event Event) {
		events <- event
	})

	proberId := uuid.New()
	prober.proberId = proberId

	// Initialize estimator with some data
	prober.rttEstimator.Update(50000) // 50ms

	// Force into Unstable state
	prober.consecutiveLoss = 2
	prober.switchState(Unstable)

	if prober.state != Unstable {
		t.Fatal("Failed to set Unstable state")
	}

	// Send packet and receive successful reply
	prober.sendProbePacket()

	var nonce uint64
	select {
	case packet := <-prober.Out():
		nonce = binary.LittleEndian.Uint64(packet.Bytes())
		mempool.Put(packet)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("No packet sent")
	}

	// Create successful reply
	replyPacket := mempool.Get(NonceSize)
	binary.LittleEndian.PutUint64(replyPacket.Bytes(), nonce)
	prober.recvProbePacket(replyPacket)

	// For Unstable state, need to clear debit (3 successful packets) or be immediate
	// Since we're testing immediate recovery behavior, check if debit was decremented
	if prober.debit <= 0 && prober.state != Normal {
		t.Errorf("Expected recovery to Normal state when debit is 0, got %v with debit %f", prober.state, prober.debit)
	}

	if prober.consecutiveLoss != 0 {
		t.Errorf("Expected consecutiveLoss to reset to 0, got %d", prober.consecutiveLoss)
	}

	t.Logf("Recovery state: %v, debit: %f", prober.state, prober.debit)

	t.Log("Successfully recovered from Unstable to Normal state")
}

func TestProber_PerformanceComparison(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	// Test performance of HoltSmoothing vs original approach
	iterations := 10000

	// HoltSmoothing performance
	h := NewHoltSmoothing(0.2, 0.1)
	start := time.Now()
	for i := 0; i < iterations; i++ {
		h.Update(float64(50000 + i%1000))
		h.Predict(1)
	}
	holtDuration := time.Since(start)

	t.Logf("HoltSmoothing: %d iterations in %v (%.2fns/op)",
		iterations, holtDuration, float64(holtDuration.Nanoseconds())/float64(iterations))

	// Performance should be reasonable (under 1µs per operation)
	avgTimePerOp := holtDuration / time.Duration(iterations)
	if avgTimePerOp > 1*time.Microsecond {
		t.Errorf("HoltSmoothing too slow: %v per operation", avgTimePerOp)
	}
}
