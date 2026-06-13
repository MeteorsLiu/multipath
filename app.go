package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tun"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/recv"
	tunnelruntime "github.com/MeteorsLiu/multipath/internal/tunnel/v2/runtime"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

var (
	errMissingServerListen = errors.New("missing server.listen")
	errMissingRemotePaths  = errors.New("missing client.remotePaths")
	errTooManyRemotePaths  = errors.New("too many client.remotePaths")
)

func runWithConfig(ctx context.Context, cfg Config) error {
	device, err := tun.Open(cfg.Tun.Name, cfg.Tun.MTU)
	if err != nil {
		return err
	}
	defer device.Close()

	if err := tun.Configure(device.Name(), cfg.Tun.LocalAddr, cfg.Tun.RemoteAddr, cfg.Tun.MTU); err != nil {
		return err
	}
	if err := tun.ConfigureRoutes(device.Name(), cfg.Tun.AllowedIPs); err != nil {
		return err
	}

	app, closers, err := buildRuntime(cfg, device)
	if err != nil {
		return err
	}
	defer closeAll(closers)

	return app.Run(ctx)
}

func buildRuntime(cfg Config, device *tun.Device) (*appRuntime, []io.Closer, error) {
	if cfg.IsServerSide {
		return buildServerRuntime(cfg, device)
	}
	return buildClientRuntime(cfg, device)
}

func buildServerRuntime(cfg Config, device *tun.Device) (*appRuntime, []io.Closer, error) {
	if cfg.Server.ListenAddr == "" {
		return nil, nil, errMissingServerListen
	}

	udpConn, err := net.ListenPacket("udp", cfg.Server.ListenAddr)
	if err != nil {
		return nil, nil, err
	}
	tcpListener, err := net.Listen("tcp", cfg.Server.ListenAddr)
	if err != nil {
		_ = udpConn.Close()
		return nil, nil, err
	}

	packetTransport, err := transport.NewPacket(transport.PacketEndpoint{
		ID:   "server",
		Conn: udpConn,
	})
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		return nil, nil, err
	}
	streamTransport := transport.NewStream(tcpListener)
	metricsServer, err := newMetricsServer(cfg)
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		return nil, nil, err
	}
	sessions := &session.Manager{}
	in := send.New(send.Config{
		StreamTransport:      streamTransport,
		SessionManager:       sessions,
		ProbeInterval:        cfg.probeInterval(),
		ProbeTimeout:         cfg.probeTimeout(),
		IsClient:             false, // server: gate starts in the Remote phase (spec 7.5)
		EnableBandwidthProbe: true,
		BWCapBps:             cfg.bandwidthProbeCapForSend(),
	})
	if cfg.FEC {
		in.EnableFEC()
	}
	out := recv.New(recv.Config{
		Handler:        tunnelruntime.NewRecvHandler(in, sessions),
		SessionManager: sessions,
	})
	return &appRuntime{
		tunReader:       device,
		tunWriter:       device,
		send:            in,
		recv:            out,
		packetTransport: packetTransport,
		streamTransport: streamTransport,
		metricsServer:   metricsServer,
	}, appendClosers([]io.Closer{udpConn, tcpListener}, metricsServer), nil
}

func buildClientRuntime(cfg Config, device *tun.Device) (*appRuntime, []io.Closer, error) {
	if len(cfg.Client.RemotePaths) == 0 {
		return nil, nil, errMissingRemotePaths
	}
	if len(cfg.Client.RemotePaths) > 254 {
		return nil, nil, errTooManyRemotePaths
	}

	var closers []io.Closer
	var endpoints []transport.PacketEndpoint
	var bootstrap []send.BootstrapLane
	streamTransport := transport.NewStream(nil)
	for i, path := range cfg.Client.RemotePaths {
		laneID := uint8(i + 1)
		conn, err := net.ListenPacket("udp", ":0")
		if err != nil {
			closeAll(closers)
			return nil, nil, err
		}
		closers = append(closers, conn)
		endpointID := fmt.Sprintf("lane-%d", laneID)
		endpoints = append(endpoints, transport.PacketEndpoint{
			ID:   endpointID,
			Conn: conn,
		})
		remote, err := net.ResolveUDPAddr("udp", path.RemoteAddr)
		if err != nil {
			closeAll(closers)
			return nil, nil, err
		}
		bootstrap = append(bootstrap, send.BootstrapLane{
			LaneID: laneID,
			Weight: uint32(path.Weight),
			Leg: transport.LegRef{
				Kind:       transport.KindUDP,
				EndpointID: endpointID,
				RemoteAddr: remote,
			},
			TCPRemote: path.RemoteAddr,
		})
	}

	var packetTransport *transport.Packet
	if len(endpoints) > 0 {
		var err error
		packetTransport, err = transport.NewPacket(endpoints...)
		if err != nil {
			closeAll(closers)
			return nil, nil, err
		}
	}
	metricsServer, err := newMetricsServer(cfg)
	if err != nil {
		closeAll(closers)
		return nil, nil, err
	}
	sessions := &session.Manager{}
	in := send.New(send.Config{
		StreamTransport:      streamTransport,
		SessionManager:       sessions,
		ProbeInterval:        cfg.probeInterval(),
		ProbeTimeout:         cfg.probeTimeout(),
		IsClient:             true, // client sent HELLO: gate starts in the Local phase (spec 7.5)
		EnableBandwidthProbe: true,
		BWCapBps:             cfg.bandwidthProbeCapForSend(),
		BootstrapLanes:       bootstrap,
	})
	if cfg.FEC {
		in.EnableFEC()
	}
	out := recv.New(recv.Config{
		Handler:        tunnelruntime.NewRecvHandler(in, sessions),
		SessionManager: sessions,
	})
	return &appRuntime{
		tunReader:       device,
		tunWriter:       device,
		send:            in,
		recv:            out,
		packetTransport: packetTransport,
		streamTransport: streamTransport,
		metricsServer:   metricsServer,
	}, appendClosers(closers, metricsServer), nil
}

func newMetricsServer(cfg Config) (*metrics.Server, error) {
	role := "client"
	if cfg.IsServerSide {
		role = "server"
	}
	metrics.SetGauge(metrics.RuntimeInfo, 1,
		metrics.L("role", role),
		metrics.L("fec", cfg.FEC),
		metrics.L("paths", len(cfg.Client.RemotePaths)),
	)
	server, err := metrics.NewServer(cfg.PromListenAddr)
	if err != nil {
		return nil, err
	}
	if server != nil {
		fmt.Fprintf(os.Stderr, "multipath prom listen: %s\n", server.Addr())
	}
	return server, nil
}

func appendClosers(closers []io.Closer, extra io.Closer) []io.Closer {
	if extra == nil {
		return closers
	}
	return append(closers, extra)
}

func closeAll(closers []io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}
