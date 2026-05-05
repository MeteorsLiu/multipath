package send

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	core "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

type ProbeLoopConfig struct {
	Events   <-chan core.Event
	Interval time.Duration
	Timeout  time.Duration
}

type ProbeLoop struct {
	sender   *Send
	events   <-chan core.Event
	interval time.Duration
	timeout  time.Duration
}

const (
	probeMaxLoss        = 5
	probeRecoverSuccess = 3
)

func NewProbeLoop(sender *Send, configs ...ProbeLoopConfig) *ProbeLoop {
	loop := &ProbeLoop{sender: sender}
	for _, cfg := range configs {
		if cfg.Events != nil {
			loop.events = cfg.Events
		}
		if cfg.Interval > 0 {
			loop.interval = cfg.Interval
		}
		if cfg.Timeout > 0 {
			loop.timeout = cfg.Timeout
		}
	}
	return loop
}

func (l *ProbeLoop) Bootstrap(ctx context.Context) error {
	debuglog.Printf("probe/loop", "bootstrap")
	err := l.sender.bootstrap(ctx)
	if err != nil {
		debuglog.Printf("probe/loop", "bootstrap err=%v", err)
	}
	return err
}

func (l *ProbeLoop) Run(ctx context.Context) error {
	if l.sender == nil || l.interval <= 0 {
		debuglog.Printf("probe/loop", "disabled sender_nil=%t interval=%s", l.sender == nil, l.interval)
		<-ctx.Done()
		return ctx.Err()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runnerOut := make(chan core.Event, 128)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	start := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			debuglog.Printf("probe/loop", "worker start name=%s", name)
			if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
				debuglog.Printf("probe/loop", "worker error name=%s err=%v", name, err)
				select {
				case errCh <- err:
				case <-runCtx.Done():
				}
			}
			debuglog.Printf("probe/loop", "worker stop name=%s", name)
		}()
	}
	if l.events != nil {
		runner := core.New(core.Config{
			Interval:       l.interval,
			Timeout:        l.timeout,
			MaxLoss:        probeMaxLoss,
			RecoverSuccess: probeRecoverSuccess,
		})
		start("core", func() error { return runner.Run(runCtx, l.events, runnerOut) })
	}
	start("adapter", func() error { return l.run(runCtx, runnerOut) })

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

func (l *ProbeLoop) run(ctx context.Context, events <-chan core.Event) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				debuglog.Printf("probe/loop", "core_events closed")
				continue
			}
			debuglog.Printf("probe/loop", "event %s", debugProbeEvent(event))
			if err := l.sender.handleProbeEvent(ctx, event); err != nil {
				debuglog.Printf("probe/loop", "event err=%v", err)
				return err
			}
		case now := <-ticker.C:
			debuglog.Printf("probe/loop", "retry_hello now_ms=%d", now.UnixMilli())
			if err := l.sender.retryOpenHELLO(ctx, uint64(now.UnixMilli())); err != nil {
				debuglog.Printf("probe/loop", "retry_hello err=%v", err)
				return err
			}
			l.sender.retryFallbackDials(ctx)
			l.sender.probeBandwidth(ctx, now)
		}
	}
}
