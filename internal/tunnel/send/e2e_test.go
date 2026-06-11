package send

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	tunio "github.com/MeteorsLiu/multipath/internal/tun"
	recvpkg "github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send/leg"
)

func TestEndToEndUDPDataAcrossTwoLanes(t *testing.T) {
	serverConn := listenPacket(t)
	defer serverConn.Close()

	clientConn1 := listenPacket(t)
	defer clientConn1.Close()
	clientConn2 := listenPacket(t)
	defer clientConn2.Close()

	serverPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "server", Conn: serverConn})
	clientPacket := newPacketTransport(t,
		transport.PacketEndpoint{ID: "lane-1", Conn: clientConn1},
		transport.PacketEndpoint{ID: "lane-2", Conn: clientConn2},
	)

	serverTun := newE2ETUN()
	clientTun := newE2ETUN()
	serverIn := New(Config{})
	clientIn := New(Config{
		ProbeInterval: 25 * time.Millisecond,
		ProbeTimeout:  time.Second,
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 1,
				Weight: 1,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "lane-1",
					RemoteAddr: serverConn.LocalAddr(),
				},
			},
			{
				LaneID: 2,
				Weight: 1,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "lane-2",
					RemoteAddr: serverConn.LocalAddr(),
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := runE2ERuntimeAsync(ctx, serverIn, nil, serverTun, serverPacket, nil)
	clientErr := runE2ERuntimeAsync(ctx, clientIn, clientTun, clientTun, clientPacket, nil)

	sendUntilTUNPacket(t, clientTun, serverTun, []byte("udp-data-1"), 2*time.Second)
	sendUntilTUNPacket(t, clientTun, serverTun, []byte("udp-data-2"), 2*time.Second)

	cancel()
	waitE2ERuntime(t, serverErr)
	waitE2ERuntime(t, clientErr)

	if len(serverIn.lanes) != 2 {
		t.Fatalf("server lanes = %d, want 2", len(serverIn.lanes))
	}
	sessionID, ok := clientIn.activeSession()
	if !ok || sessionID == 0 {
		t.Fatalf("client active session = (%d,%v), want generated session", sessionID, ok)
	}
	for laneID := uint8(1); laneID <= 2; laneID++ {
		lane := serverIn.lanes[laneKey{sessionID: sessionID, laneID: laneID}]
		if lane == nil || !lane.udpReady {
			t.Fatalf("server lane %d ready = %v, lane=%+v", laneID, lane != nil && lane.udpReady, lane)
		}
	}
}

func TestEndToEndFECRecoversOneDroppedUDPPacket(t *testing.T) {
	target := ipv4TestPacket(61)
	serverRaw := listenPacket(t)
	defer serverRaw.Close()
	serverConn := &dropDataPacketConn{
		PacketConn: serverRaw,
		packet:     target,
		dropped:    make(chan struct{}),
	}

	clientConn := listenPacket(t)
	defer clientConn.Close()

	serverPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "server", Conn: serverConn})
	clientPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "lane-1", Conn: clientConn})
	serverTun := newE2ETUN()
	clientTun := newE2ETUN()
	serverIn := New(Config{EnableFEC: true})
	clientIn := New(Config{
		ProbeInterval: 25 * time.Millisecond,
		ProbeTimeout:  time.Second,
		EnableFEC:     true,
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 1,
				Weight: 1,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "lane-1",
					RemoteAddr: serverRaw.LocalAddr(),
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := runE2ERuntimeAsync(ctx, serverIn, nil, serverTun, serverPacket, nil)
	clientErr := runE2ERuntimeAsync(ctx, clientIn, clientTun, clientTun, clientPacket, nil)

	sendUntilTUNPacket(t, clientTun, serverTun, ipv4TestPacket(40), 2*time.Second)
	drainE2ETUN(serverTun)

	sendTUNPacket(t, clientTun, target, time.Second)
	for i := 0; i < 8; i++ {
		sendTUNPacket(t, clientTun, ipv4TestPacket(44+i), time.Second)
	}
	waitDropped(t, serverConn.dropped, 2*time.Second)
	waitTUNPacket(t, serverTun, target, 2*time.Second)

	cancel()
	waitE2ERuntime(t, serverErr)
	waitE2ERuntime(t, clientErr)
}

func TestEndToEndTCPFallbackAfterUDPHELLOTimeout(t *testing.T) {
	blackholeConn := listenPacket(t)
	defer blackholeConn.Close()

	clientUDP := listenPacket(t)
	defer clientUDP.Close()
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp: %v", err)
	}
	defer serverListener.Close()

	serverStream := transport.NewStream(serverListener)
	clientPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "lane-1", Conn: clientUDP})
	clientStream := transport.NewStream(nil)
	serverTun := newE2ETUN()
	clientTun := newE2ETUN()
	serverIn := New(Config{
		StreamTransport: serverStream,
	})
	clientIn := New(Config{
		StreamTransport: clientStream,
		ProbeInterval:   20 * time.Millisecond,
		ProbeTimeout:    60 * time.Millisecond,
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 1,
				Weight: 1,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "lane-1",
					RemoteAddr: blackholeConn.LocalAddr(),
				},
				TCPRemote: serverListener.Addr().String(),
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := runE2ERuntimeAsync(ctx, serverIn, nil, serverTun, nil, serverStream)
	clientErr := runE2ERuntimeAsync(ctx, clientIn, clientTun, clientTun, clientPacket, clientStream)

	sendUntilTUNPacket(t, clientTun, serverTun, []byte("tcp-fallback-data"), 3*time.Second)

	cancel()
	waitE2ERuntime(t, serverErr)
	waitE2ERuntime(t, clientErr)

	sessionID, ok := clientIn.activeSession()
	if !ok || sessionID == 0 {
		t.Fatalf("client active session = (%d,%v), want generated session", sessionID, ok)
	}
	lane := serverIn.lanes[laneKey{sessionID: sessionID, laneID: 1}]
	if lane == nil || !lane.tcpReady {
		t.Fatalf("server TCP fallback lane ready = %v, lane=%+v", lane != nil && lane.tcpReady, lane)
	}
}

func TestEndToEndBandwidthProbeSelectsTCPWhenUDPQoSLimited(t *testing.T) {
	serverRaw := listenPacket(t)
	defer serverRaw.Close()
	serverConn := &qosBandwidthProbePacketConn{PacketConn: serverRaw}

	clientRaw := listenPacket(t)
	defer clientRaw.Close()
	clientConn := &qosBandwidthProbePacketConn{PacketConn: clientRaw}
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp: %v", err)
	}
	defer serverListener.Close()

	serverPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "server", Conn: serverConn})
	clientPacket := newPacketTransport(t, transport.PacketEndpoint{ID: "lane-1", Conn: clientConn})
	serverStream := transport.NewStream(serverListener)
	clientStream := transport.NewStream(nil)
	serverTun := newE2ETUN()
	clientTun := newE2ETUN()
	serverIn := New(Config{
		StreamTransport: serverStream,
		ProbeInterval:   20 * time.Millisecond,
		ProbeTimeout:    time.Second,
	})
	clientIn := New(Config{
		StreamTransport: clientStream,
		ProbeInterval:   20 * time.Millisecond,
		ProbeTimeout:    time.Second,
		BootstrapLanes: []BootstrapLane{
			{
				LaneID: 1,
				Weight: 1,
				Leg: transport.LegRef{
					Kind:       transport.KindUDP,
					EndpointID: "lane-1",
					RemoteAddr: serverRaw.LocalAddr(),
				},
				TCPRemote: serverListener.Addr().String(),
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	serverErr := runE2ERuntimeAsync(ctx, serverIn, serverTun, serverTun, serverPacket, serverStream)
	clientErr := runE2ERuntimeAsync(ctx, clientIn, clientTun, clientTun, clientPacket, clientStream)

	sendUntilTUNPacket(t, clientTun, serverTun, []byte("bootstrap-data"), 2*time.Second)
	sessionID, ok := clientIn.activeSession()
	if !ok || sessionID == 0 {
		t.Fatalf("client active session = (%d,%v), want generated session", sessionID, ok)
	}
	key := laneKey{sessionID: sessionID, laneID: 1}
	waitForE2ELane(t, clientIn, key, func(lane *laneRuntime) bool {
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return lane.udpReady && lane.tcpReady
	}, 3*time.Second)
	waitForE2ELane(t, serverIn, key, func(lane *laneRuntime) bool {
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return lane.udpReady && lane.tcpReady
	}, 3*time.Second)

	waitForE2ELane(t, clientIn, key, func(lane *laneRuntime) bool {
		_, udpQ, _, tcpQ := lane.legQualities()
		return udpQ.BandwidthQoSLimited && udpQ.BandwidthPreferTCP && tcpQ.ProbeSamples >= leg.MinBandwidthProbeSamples
	}, 35*time.Second)

	serverConn.dropData.Store(true)
	sendUntilTUNPacket(t, clientTun, serverTun, []byte("tcp-selected-after-qos"), 3*time.Second)

	waitForE2ELane(t, serverIn, key, func(lane *laneRuntime) bool {
		_, udpQ, _, tcpQ := lane.legQualities()
		return udpQ.BandwidthQoSLimited && udpQ.BandwidthPreferTCP && tcpQ.ProbeSamples >= leg.MinBandwidthProbeSamples
	}, 35*time.Second)

	clientConn.dropData.Store(true)
	sendUntilTUNPacket(t, serverTun, clientTun, []byte("server-tcp-selected-after-qos"), 3*time.Second)

	cancel()
	waitE2ERuntime(t, serverErr)
	waitE2ERuntime(t, clientErr)
}

type e2eTUN struct {
	in  chan []byte
	out chan []byte
}

func newE2ETUN() *e2eTUN {
	return &e2eTUN{
		in:  make(chan []byte, 128),
		out: make(chan []byte, 128),
	}
}

func (t *e2eTUN) ReadPacket(ctx context.Context) (*packetbuf.Packet, error) {
	select {
	case payload := <-t.in:
		packet := packetbuf.Acquire(len(payload))
		copy(packet.Payload, payload)
		return packet, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *e2eTUN) WritePacket(ctx context.Context, packet []byte) (int, error) {
	payload := append([]byte(nil), packet...)
	select {
	case t.out <- payload:
		return len(packet), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type dropDataPacketConn struct {
	net.PacketConn
	packet  []byte
	dropped chan struct{}
	once    sync.Once
}

type qosBandwidthProbePacketConn struct {
	net.PacketConn
	dropData atomic.Bool
}

func (c *qosBandwidthProbePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if c.shouldDrop(p[:n]) {
			continue
		}
		return n, addr, nil
	}
}

func (c *qosBandwidthProbePacketConn) shouldDrop(payload []byte) bool {
	frame, err := protocol.Decode(payload)
	if err != nil {
		return false
	}
	switch body := frame.Body.(type) {
	case protocol.BandwidthProbeBody:
		return frame.Type == protocol.TypeBandwidthProbe && body.Seq != 0 && body.Seq+1 != body.Count
	case protocol.DataBody:
		return c.dropData.Load()
	default:
		return false
	}
}

func (c *dropDataPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if c.shouldDrop(p[:n]) {
			c.once.Do(func() {
				close(c.dropped)
			})
			continue
		}
		return n, addr, nil
	}
}

func (c *dropDataPacketConn) shouldDrop(payload []byte) bool {
	frame, err := protocol.Decode(payload)
	if err != nil || frame.Type != protocol.TypeDATA {
		return false
	}
	body, ok := frame.Body.(protocol.DataBody)
	return ok && bytes.Equal(body.Packet, c.packet)
}

func listenPacket(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	return conn
}

func newPacketTransport(t *testing.T, endpoints ...transport.PacketEndpoint) *transport.Packet {
	t.Helper()
	packetTransport, err := transport.NewPacket(endpoints...)
	if err != nil {
		t.Fatalf("NewPacket: %v", err)
	}
	return packetTransport
}

func runE2ERuntimeAsync(ctx context.Context, in *Send, tunReader tunio.PacketReader, tunWriter *e2eTUN, packetTransport transport.PacketTransport, streamTransport transport.StreamTransport) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- runE2ERuntime(ctx, in, tunReader, tunWriter, packetTransport, streamTransport)
	}()
	return errCh
}

func runE2ERuntime(ctx context.Context, in *Send, tunReader tunio.PacketReader, tunWriter *e2eTUN, packetTransport transport.PacketTransport, streamTransport transport.StreamTransport) error {
	if err := in.bootstrap(ctx); err != nil {
		return err
	}
	if setter, ok := streamTransport.(interface {
		SetFailureHandler(transport.LegFailureHandler)
	}); ok {
		setter.SetFailureHandler(in)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	start := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errCh <- err:
				case <-runCtx.Done():
				}
			}
		}()
	}

	if in.probeInterval > 0 {
		start(func() error {
			return testProbeLoopRunner{
				send:     in,
				events:   in.probeEvents,
				interval: in.probeInterval,
				timeout:  in.probeTimeout,
			}.Run(runCtx)
		})
	}
	if tunReader != nil {
		start(func() error { return tunio.Run(runCtx, tunReader, testSendPacketWriter{send: in}) })
	}
	start(func() error { return transport.RunWriter(runCtx, in.Packets(), packetTransport, streamTransport) })
	out := recvpkg.New(recvpkg.Config{Control: NewRecvState(in), SessionManager: in.sessionManager})
	if tunWriter != nil {
		start(func() error { return tunio.RunWriter(runCtx, out.Packets(), tunWriter) })
	}
	if packetTransport != nil {
		start(func() error { return packetTransport.Run(runCtx, out) })
	}
	if streamTransport != nil {
		start(func() error { return streamTransport.Run(runCtx, out) })
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		cancel()
		<-done
		return ctx.Err()
	case err := <-errCh:
		cancel()
		<-done
		return err
	case <-done:
		return nil
	}
}

func waitE2ERuntime(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("runtime err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runtime exit")
	}
}

func sendUntilTUNPacket(t *testing.T, src *e2eTUN, dst *e2eTUN, packet []byte, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	sendTUNPacket(t, src, packet, timeout)
	for {
		select {
		case got := <-dst.out:
			if bytes.Equal(got, packet) {
				return
			}
		case <-ticker.C:
			select {
			case src.in <- append([]byte(nil), packet...):
			default:
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for TUN packet %x", packet)
		}
	}
}

func sendTUNPacket(t *testing.T, tun *e2eTUN, packet []byte, timeout time.Duration) {
	t.Helper()
	select {
	case tun.in <- append([]byte(nil), packet...):
	case <-time.After(timeout):
		t.Fatalf("timed out sending TUN packet %x", packet)
	}
}

func waitTUNPacket(t *testing.T, tun *e2eTUN, packet []byte, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case got := <-tun.out:
			if bytes.Equal(got, packet) {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for TUN packet %x", packet)
		}
	}
}

func waitDropped(t *testing.T, dropped <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-dropped:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for packet drop")
	}
}

func waitForE2ELane(t *testing.T, in *Send, key laneKey, ok func(*laneRuntime) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		lane := in.getLane(key)
		if lane != nil && ok(lane) {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for lane session=%d lane=%d", key.sessionID, key.laneID)
		}
	}
}

func drainE2ETUN(tun *e2eTUN) {
	for {
		select {
		case <-tun.out:
		default:
			return
		}
	}
}
