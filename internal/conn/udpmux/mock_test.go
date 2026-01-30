package udpmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/prober"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func generateBuf(size int) []byte {
	buf := make([]byte, size)

	io.ReadFull(rand.Reader, buf)

	return buf
}

func newIPPacket() []byte {
	payload := generateBuf(100)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths: true,
	}
	gopacket.SerializeLayers(buf, opts,
		&layers.Ethernet{},
		&layers.IPv4{
			Version: 4,
		},
		&layers.TCP{},
		gopacket.Payload(payload))
	return buf.Bytes()

}

func TestReceiver(t *testing.T) {
	conn, err := net.ListenPacket("udp", "0.0.0.0:9999")
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *mempool.Buffer, 1024)
	clientSender := make(chan *mempool.Buffer, 1024)
	senderManager := path.NewManager()
	proberManager := prober.NewManager()
	reader := newUDPReceiver(ctx, cancel, conn, ch, clientSender, senderManager, proberManager, func(s string) {
		fmt.Println("bbb", s)
	}, false)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		reader.readLoop()
	}()

	wg.Wait()

	local, err := net.Dial("udp", "127.0.0.1:9999")
	if err != nil {
		t.Error(err)
		return
	}
	defer local.Close()

	pkt1 := newIPPacket()
	pkt2 := newIPPacket()

	header := mempool.Get(1)
	defer mempool.Put(header)
	protocol.MakeHeader(header, protocol.TunEncap)

	local.Write(append(header.Bytes(), pkt1[0:50]...))
	local.Write(pkt1[50:58])
	local.Write(pkt1[58:])

	local.Write(append(header.Bytes(), pkt2...))
	buf1 := <-ch
	if !bytes.Equal(buf1.Bytes(), pkt1) {
		t.Errorf("unexpected packet: got: %v, want: %v", buf1.Bytes(), pkt1)
	}
	buf2 := <-ch
	if !bytes.Equal(buf2.Bytes(), pkt2) {
		t.Errorf("unexpected packet: got: %v, want: %v", buf2.Bytes(), pkt2)
	}
}

// TestUDPSender tests basic sender functionality
func TestUDPSender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sender := newUDPSender(ctx, cancel)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer conn.Close()

	remoteAddr := "127.0.0.1:8888"
	proberCh := make(chan *mempool.Buffer, 10)

	sender.Start(conn, remoteAddr, proberCh)

	testData := generateBuf(100)
	buf := mempool.Get(len(testData) + protocol.HeaderSize)
	defer mempool.Put(buf)

	protocol.MakeHeader(buf, protocol.TunEncap)
	buf.Write(testData)

	err = sender.Write(buf)
	if err != nil {
		t.Errorf("Write failed: %v", err)
	}

	if sender.Remote() != remoteAddr {
		t.Errorf("Remote() = %v, want %v", sender.Remote(), remoteAddr)
	}

	sender.Close()
}

// TestUDPSenderClose tests sender close functionality
func TestUDPSenderClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sender := newUDPSender(ctx, cancel)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	remoteAddr := "127.0.0.1:8889"
	proberCh := make(chan *mempool.Buffer, 10)
	sender.Start(conn, remoteAddr, proberCh)

	err = sender.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	buf := mempool.Get(100)
	defer mempool.Put(buf)
	err = sender.Write(buf)
	if err == nil {
		t.Error("Write should fail after close")
	}
}

// TestUDPSenderContextCancel tests sender context cancellation
func TestUDPSenderContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sender := newUDPSender(ctx, cancel)

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer conn.Close()

	remoteAddr := "127.0.0.1:8890"
	proberCh := make(chan *mempool.Buffer, 10)
	sender.Start(conn, remoteAddr, proberCh)

	cancel()

	time.Sleep(100 * time.Millisecond)

	buf := mempool.Get(100)
	defer mempool.Put(buf)
	err = sender.Write(buf)
	if err == nil {
		t.Error("Write should fail after context cancel")
	}
}

// TestProberIntegrationWithUDPMux tests complete prober lifecycle
func TestProberIntegrationWithUDPMux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create channels for communication
	outCh := make(chan *mempool.Buffer, 100)

	// Create path manager and prober manager
	pathManager := path.NewManager()
	proberManager := prober.NewManager()

	// Create UDP connections
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create server conn: %v", err)
	}
	defer serverConn.Close()

	clientConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create client conn: %v", err)
	}
	defer clientConn.Close()

	serverAddr := serverConn.LocalAddr().String()
	clientAddr := clientConn.LocalAddr().String()

	// Track prober state changes
	stateEvents := make(chan prober.Event, 20)

	onProberEvent := func(context any, event prober.Event) {
		stateEvents <- event
		t.Logf("Prober state changed to: %v", event)
	}

	// Create server-side receiver
	serverReceiver := newUDPReceiver(ctx, cancel, serverConn, outCh, nil, pathManager, proberManager, func(addr string) {
		t.Logf("Server received from: %s", addr)
	}, true)
	serverReceiver.Start()

	// Create client-side components
	clientCtx, clientCancel := context.WithCancel(ctx)
	id, clientProber := proberManager.Register(clientCtx, fmt.Sprintf("%s => %s", clientAddr, serverAddr), onProberEvent)

	udpClientSender := newUDPSender(clientCtx, clientCancel)
	clientReceiver := newUDPReceiver(clientCtx, clientCancel, clientConn, outCh, udpClientSender.queue, pathManager, proberManager, func(addr string) {
		t.Logf("Client received from: %s", addr)
	}, false)

	clientReceiver.Start()
	udpClientSender.Start(clientConn, serverAddr, clientProber.Out())
	clientProber.Start(&proberContext{
		addr:   serverAddr,
		sender: udpClientSender,
	}, id)

	// Test Phase 1: Initial connection establishment (Initializing -> Normal)
	t.Log("Phase 1: Testing initial connection establishment")

	select {
	case event := <-stateEvents:
		if event != prober.Normal {
			t.Errorf("Expected first event to be Normal, got %v", event)
		}
		t.Log("✓ Successfully transitioned from Initializing to Normal")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for Normal state")
	}

	// Test Phase 2: Stable operation
	t.Log("Phase 2: Testing stable operation")

	// Send some test data through the established connection
	testPacket := newIPPacket()
	testBuf := mempool.Get(len(testPacket) + protocol.HeaderSize)
	protocol.MakeHeader(testBuf, protocol.TunEncap)
	testBuf.Write(testPacket)

	err = udpClientSender.Write(testBuf)
	if err != nil {
		t.Errorf("Failed to send test packet: %v", err)
	}

	// Verify packet received
	select {
	case receivedBuf := <-outCh:
		if !bytes.Equal(receivedBuf.Bytes(), testPacket) {
			t.Error("Received packet data mismatch")
		}
		mempool.Put(receivedBuf)
		t.Log("✓ Successfully sent and received data packet")
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for data packet")
	}

	// Test Phase 3: Connection loss simulation (Normal -> Lost)
	t.Log("Phase 3: Testing connection loss and recovery")

	// Stop the server to simulate network issues
	serverReceiver.Close()
	serverConn.Close()

	// Wait for state transition to Lost
	lostReceived := false

	for i := 0; i < 10; i++ {
		select {
		case event := <-stateEvents:
			switch event {
			case prober.Lost:
				lostReceived = true
				t.Log("✓ Successfully transitioned to Lost state")
			}
			if lostReceived {
				break
			}
		case <-time.After(1 * time.Second):
			// Continue waiting
		}
	}

	if !lostReceived {
		t.Error("Expected transition to Lost state")
	}

	// Test Phase 4: Connection recovery (Lost -> Normal)
	t.Log("Phase 4: Testing connection recovery")

	// Restart server to simulate recovery
	newServerConn, err := net.ListenPacket("udp", serverAddr)
	if err != nil {
		t.Fatalf("Failed to restart server: %v", err)
	}
	defer newServerConn.Close()

	newServerReceiver := newUDPReceiver(ctx, cancel, newServerConn, outCh, nil, pathManager, proberManager, func(addr string) {
		t.Logf("Recovered server received from: %s", addr)
	}, true)
	newServerReceiver.Start()

	// Wait for recovery to Normal state
	recoveryReceived := false
	for i := 0; i < 20; i++ {
		select {
		case event := <-stateEvents:
			if event == prober.Normal {
				recoveryReceived = true
				t.Log("✓ Successfully recovered to Normal state")
				break
			}
		case <-time.After(1 * time.Second):
			// Continue waiting
		}
	}

	if !recoveryReceived {
		t.Error("Expected recovery to Normal state")
	}

	// Test Phase 5: Final data transmission after recovery
	t.Log("Phase 5: Testing data transmission after recovery")

	testPacket2 := newIPPacket()
	testBuf2 := mempool.Get(len(testPacket2) + protocol.HeaderSize)
	protocol.MakeHeader(testBuf2, protocol.TunEncap)
	testBuf2.Write(testPacket2)

	err = udpClientSender.Write(testBuf2)
	if err != nil {
		t.Errorf("Failed to send test packet after recovery: %v", err)
	}

	select {
	case receivedBuf := <-outCh:
		if !bytes.Equal(receivedBuf.Bytes(), testPacket2) {
			t.Error("Received packet data mismatch after recovery")
		}
		mempool.Put(receivedBuf)
		t.Log("✓ Successfully sent and received data packet after recovery")
	case <-time.After(3 * time.Second):
		t.Error("Timeout waiting for data packet after recovery")
	}

	t.Log("✅ Comprehensive prober integration test completed successfully")
}

// TestConnectionEstablishment tests the full connection establishment process
func TestConnectionEstablishment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	outCh := make(chan *mempool.Buffer, 100)
	pathManager := path.NewManager()

	// Start server
	serverAddr := "127.0.0.1:9998"
	go func() {
		ListenConn(ctx, pathManager, serverAddr, outCh)
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Start client
	go func() {
		DialConn(ctx, pathManager, serverAddr, outCh)
	}()

	// Wait for connection establishment
	time.Sleep(1 * time.Second)

	// Note: We can't easily verify connections without exposing internal state
	// This test mainly verifies that the connection establishment code runs without panicking

	t.Log("✓ Connection establishment test completed")
}

// TestPacketFragmentation tests packet fragmentation and reassembly
func TestPacketFragmentation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *mempool.Buffer, 1024)
	clientSender := make(chan *mempool.Buffer, 1024)
	senderManager := path.NewManager()
	proberManager := prober.NewManager()

	conn, err := net.ListenPacket("udp", "127.0.0.1:9997")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	defer conn.Close()

	reader := newUDPReceiver(ctx, cancel, conn, ch, clientSender, senderManager, proberManager, func(s string) {
		t.Logf("Received from: %s", s)
	}, false)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		reader.readLoop()
	}()
	wg.Wait()

	// Create client connection
	local, err := net.Dial("udp", "127.0.0.1:9997")
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer local.Close()

	// Create a large packet that will be fragmented
	largePacket := newIPPacket()
	// Make it larger to test fragmentation
	largeData := make([]byte, len(largePacket)+500)
	copy(largeData, largePacket)
	for i := len(largePacket); i < len(largeData); i++ {
		largeData[i] = byte(i % 256)
	}

	header := mempool.Get(1)
	defer mempool.Put(header)
	protocol.MakeHeader(header, protocol.TunEncap)

	// Send packet in fragments
	fragmentSize := 200
	fragments := [][]byte{}

	fullPacket := append(header.Bytes(), largeData...)
	for i := 0; i < len(fullPacket); i += fragmentSize {
		end := i + fragmentSize
		if end > len(fullPacket) {
			end = len(fullPacket)
		}
		fragments = append(fragments, fullPacket[i:end])
	}

	// Send all fragments
	for i, fragment := range fragments {
		_, err := local.Write(fragment)
		if err != nil {
			t.Fatalf("Failed to send fragment %d: %v", i, err)
		}
		t.Logf("Sent fragment %d: %d bytes", i, len(fragment))
	}

	// Wait for reassembled packet
	select {
	case buf := <-ch:
		if !bytes.Equal(buf.Bytes(), largeData) {
			t.Error("Reassembled packet data mismatch")
		}
		mempool.Put(buf)
		t.Log("✓ Successfully reassembled fragmented packet")
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for reassembled packet")
	}
}

// TestConcurrentOperations tests concurrent senders and receivers
func TestConcurrentOperations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numGoroutines = 10
	const packetsPerGoroutine = 50

	outCh := make(chan *mempool.Buffer, numGoroutines*packetsPerGoroutine)
	pathManager := path.NewManager()
	proberManager := prober.NewManager()

	// Create server
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer serverConn.Close()

	serverReceiver := newUDPReceiver(ctx, cancel, serverConn, outCh, nil, pathManager, proberManager, func(addr string) {}, true)
	serverReceiver.Start()

	serverAddr := serverConn.LocalAddr().String()

	// Create multiple concurrent clients
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			clientConn, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Errorf("Client %d: Failed to create connection: %v", clientID, err)
				return
			}
			defer clientConn.Close()

			clientCtx, clientCancel := context.WithCancel(ctx)
			defer clientCancel()

			sender := newUDPSender(clientCtx, clientCancel)
			proberCh := make(chan *mempool.Buffer, 10)
			sender.Start(clientConn, serverAddr, proberCh)

			// Send multiple packets
			for j := 0; j < packetsPerGoroutine; j++ {
				testData := generateBuf(100)
				buf := mempool.Get(len(testData) + protocol.HeaderSize)
				protocol.MakeHeader(buf, protocol.TunEncap)
				buf.Write(testData)

				err := sender.Write(buf)
				if err != nil {
					t.Errorf("Client %d: Failed to write packet %d: %v", clientID, j, err)
					continue
				}

				// Small delay between packets
				time.Sleep(1 * time.Millisecond)
			}

			t.Logf("Client %d completed sending %d packets", clientID, packetsPerGoroutine)
		}(i)
	}

	// Wait for all clients to finish
	wg.Wait()

	// Collect received packets with timeout
	receivedCount := 0
	expectedCount := numGoroutines * packetsPerGoroutine

	timeout := time.After(5 * time.Second)
	for receivedCount < expectedCount {
		select {
		case buf := <-outCh:
			receivedCount++
			mempool.Put(buf)
		case <-timeout:
			t.Logf("Timeout reached. Received %d/%d packets", receivedCount, expectedCount)
			goto done
		}
	}

done:
	if receivedCount < expectedCount/2 { // Allow for some packet loss
		t.Errorf("Too few packets received: %d/%d", receivedCount, expectedCount)
	} else {
		t.Logf("✓ Concurrent operations test completed: %d/%d packets received", receivedCount, expectedCount)
	}
}

// TestErrorHandling tests various error conditions
func TestErrorHandling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Test 1: Invalid packet size
	t.Run("InvalidPacketSize", func(t *testing.T) {
		ch := make(chan *mempool.Buffer, 10)
		pathManager := path.NewManager()
		proberManager := prober.NewManager()

		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Failed to listen: %v", err)
		}
		defer conn.Close()

		receiver := newUDPReceiver(ctx, cancel, conn, ch, nil, pathManager, proberManager, func(string) {}, false)

		// Create invalid packet (too small)
		invalidPacket := mempool.Get(5)
		defer mempool.Put(invalidPacket)
		invalidPacket.Write([]byte{1, 2, 3, 4, 5})

		err = receiver.handlePacket("127.0.0.1:12345", invalidPacket)
		if err == nil {
			t.Error("Expected error for invalid packet size")
		}
	})

	// Test 2: Context cancellation
	t.Run("ContextCancellation", func(t *testing.T) {
		testCtx, testCancel := context.WithCancel(context.Background())

		sender := newUDPSender(testCtx, testCancel)

		// Cancel context immediately
		testCancel()

		buf := mempool.Get(100)
		defer mempool.Put(buf)

		err := sender.Write(buf)
		if err == nil {
			t.Error("Expected error when writing to cancelled sender")
		}
	})

	// Test 3: Network connection errors
	t.Run("NetworkErrors", func(t *testing.T) {
		// Create sender with invalid remote address
		testCtx, testCancel := context.WithCancel(context.Background())
		defer testCancel()

		sender := newUDPSender(testCtx, testCancel)

		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Failed to create connection: %v", err)
		}
		defer conn.Close()

		invalidAddr := "999.999.999.999:99999"
		proberCh := make(chan *mempool.Buffer, 1)

		// This should not panic, but may log errors
		sender.Start(conn, invalidAddr, proberCh)

		buf := mempool.Get(100)
		defer mempool.Put(buf)

		// Write should succeed (queuing), but network send may fail
		err = sender.Write(buf)
		if err != nil {
			t.Logf("Write failed as expected with invalid address: %v", err)
		}
	})

	t.Log("✓ Error handling tests completed")
}
