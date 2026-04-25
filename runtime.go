package main

import (
	"context"
	"errors"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tun"
	"github.com/MeteorsLiu/multipath/internal/tunnel/probe"
	"github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

type appRuntime struct {
	tunReader       tun.PacketReader
	tunWriter       *tun.Device
	send            *send.Send
	probeLoop       *probe.Loop
	recv            *recv.Recv
	packetTransport transport.PacketTransport
	streamTransport transport.StreamTransport
}

func (r *appRuntime) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if r.probeLoop != nil {
		if err := r.probeLoop.Bootstrap(runCtx); err != nil {
			return err
		}
	}

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

	started := false
	if r.probeLoop != nil {
		started = true
		start(func() error { return r.probeLoop.Run(runCtx) })
	}
	if r.tunReader != nil {
		started = true
		start(func() error { return tun.Run(runCtx, r.tunReader, r.send) })
	}
	if r.send != nil {
		started = true
		start(func() error {
			return transport.RunWriter(runCtx, r.send.Packets(), r.packetTransport, r.streamTransport)
		})
	}
	if r.tunWriter != nil && r.recv != nil {
		started = true
		start(func() error { return tun.RunWriter(runCtx, r.recv.Packets(), r.tunWriter) })
	}
	if r.packetTransport != nil {
		started = true
		start(func() error { return r.packetTransport.Run(runCtx, r.recv) })
	}
	if r.streamTransport != nil {
		started = true
		start(func() error { return r.streamTransport.Run(runCtx, r.recv) })
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
