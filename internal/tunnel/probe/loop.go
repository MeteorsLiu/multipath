package probe

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	core "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
	"github.com/MeteorsLiu/multipath/internal/tunnel/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send"
)

type Config struct {
	Events   <-chan core.Event
	Interval time.Duration
	Timeout  time.Duration
}

type Loop struct {
	sender   *send.Send
	events   <-chan core.Event
	interval time.Duration
	timeout  time.Duration
}

func New(sender *send.Send, configs ...Config) *Loop {
	loop := &Loop{sender: sender}
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

func (l *Loop) Bootstrap(ctx context.Context) error {
	return l.sender.Bootstrap(ctx)
}

func (l *Loop) Write(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (recv.Result, error) {
	result, err := l.sender.WriteFrame(ctx, frame, leg)
	return recv.Result{
		Accepted:   result.Accepted,
		Caps:       result.Caps,
		FECProfile: result.FECProfile,
	}, err
}

func (l *Loop) Run(ctx context.Context) error {
	if l.sender == nil || l.interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runnerOut := make(chan core.Event, 128)
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
	if l.events != nil {
		runner := core.New(core.Config{
			Interval:       l.interval,
			Timeout:        l.timeout,
			MaxLoss:        1,
			RecoverSuccess: 1,
		})
		start(func() error { return runner.Run(runCtx, l.events, runnerOut) })
	}
	start(func() error { return l.run(runCtx, runnerOut) })

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

func (l *Loop) run(ctx context.Context, events <-chan core.Event) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := l.sender.WriteProbeEvent(ctx, event); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := l.sender.RetryHELLO(ctx, uint64(now.UnixMilli())); err != nil {
				return err
			}
		}
	}
}
