package main

import (
	"context"
	"errors"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tun"
	"github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

type appRuntime struct {
	tunReader       tun.PacketReader
	tunWriter       *tun.Device
	send            *send.Send
	probeLoop       *send.ProbeLoop
	recv            *recv.Recv
	packetTransport transport.PacketTransport
	streamTransport transport.StreamTransport
	metricsServer   *metrics.Server
}

func (r *appRuntime) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if r.probeLoop != nil {
		debuglog.Printf("runtime", "bootstrap")
		if err := r.probeLoop.Bootstrap(runCtx); err != nil {
			debuglog.Printf("runtime", "bootstrap err=%v", err)
			return err
		}
	}

	errCh := make(chan error, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	start := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			debuglog.Printf("runtime", "loop start name=%s", name)
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
				debuglog.Printf("runtime", "loop error name=%s err=%v", name, err)
				select {
				case errCh <- err:
				case <-runCtx.Done():
				}
			}
			debuglog.Printf("runtime", "loop stop name=%s", name)
		}()
	}

	started := false
	if r.probeLoop != nil {
		started = true
		start("probe", func() error { return r.probeLoop.Run(runCtx) })
	}
	if r.metricsServer != nil {
		started = true
		start("prom", func() error { return r.metricsServer.Run(runCtx) })
	}
	if r.tunReader != nil {
		started = true
		start("tun-read", func() error { return tun.Run(runCtx, r.tunReader, r.send) })
	}
	if r.send != nil {
		started = true
		start("transport-writer", func() error {
			return transport.RunWriter(runCtx, r.send.Packets(), r.packetTransport, r.streamTransport)
		})
	}
	if r.tunWriter != nil && r.recv != nil {
		started = true
		start("tun-write", func() error { return tun.RunWriter(runCtx, r.recv.Packets(), r.tunWriter) })
	}
	if r.packetTransport != nil {
		started = true
		start("udp-read", func() error { return r.packetTransport.Run(runCtx, r.recv) })
	}
	if r.streamTransport != nil {
		started = true
		start("tcp-read", func() error { return r.streamTransport.Run(runCtx, r.recv) })
	}

	if !started {
		<-runCtx.Done()
		return runCtx.Err()
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
