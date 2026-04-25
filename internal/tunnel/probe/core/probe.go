package core

import (
	"context"
	"time"
)

type Target uint64

type EventType uint8

const (
	EventTrack EventType = iota + 1
	EventUntrack
	EventSendPing
	EventPingFailed
	EventPongReceived
	EventTargetLost
	EventTargetRecovered
)

type Event struct {
	Type   EventType
	Target Target
	PingID uint64
	TimeMS uint64
}

type Config struct {
	Interval       time.Duration
	Timeout        time.Duration
	MaxLoss        int
	RecoverSuccess int
}

type Runner struct {
	config     Config
	targets    map[Target]*targetRuntime
	nextPingID uint64
}

type targetRuntime struct {
	pending        map[uint64]uint64
	lost           bool
	lossCount      int
	recoverSuccess int
}

func New(config Config) *Runner {
	if config.MaxLoss <= 0 {
		config.MaxLoss = 1
	}
	if config.RecoverSuccess <= 0 {
		config.RecoverSuccess = 1
	}
	return &Runner{
		config:  config,
		targets: make(map[Target]*targetRuntime),
	}
}

func (r *Runner) Run(ctx context.Context, in <-chan Event, out chan<- Event) error {
	if r.config.Interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	ticker := time.NewTicker(r.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			if !r.handleInput(ctx, out, event) {
				return ctx.Err()
			}
		case now := <-ticker.C:
			if !r.tick(ctx, out, uint64(now.UnixMilli())) {
				return ctx.Err()
			}
		}
	}
}

func (r *Runner) handleInput(ctx context.Context, out chan<- Event, event Event) bool {
	switch event.Type {
	case EventTrack:
		if event.Target != 0 && r.targets[event.Target] == nil {
			r.targets[event.Target] = &targetRuntime{pending: make(map[uint64]uint64)}
		}
	case EventUntrack:
		delete(r.targets, event.Target)
	case EventPingFailed:
		targetInfo := r.targets[event.Target]
		if targetInfo == nil {
			return true
		}
		delete(targetInfo.pending, event.PingID)
		return r.markLost(ctx, out, event.Target, targetInfo)
	case EventPongReceived:
		return r.handlePONG(ctx, out, event)
	}
	return true
}

func (r *Runner) tick(ctx context.Context, out chan<- Event, nowMS uint64) bool {
	for target, targetInfo := range r.targets {
		if r.config.Timeout > 0 && !r.expire(ctx, out, target, targetInfo, nowMS) {
			return false
		}
		pingID := r.nextPingID
		r.nextPingID++
		targetInfo.pending[pingID] = nowMS
		if !emit(ctx, out, Event{
			Type:   EventSendPing,
			Target: target,
			PingID: pingID,
			TimeMS: nowMS,
		}) {
			return false
		}
	}
	return true
}

func (r *Runner) expire(ctx context.Context, out chan<- Event, target Target, targetInfo *targetRuntime, nowMS uint64) bool {
	timedOut := false
	for pingID, timeMS := range targetInfo.pending {
		if nowMS < timeMS {
			continue
		}
		if time.Duration(nowMS-timeMS)*time.Millisecond < r.config.Timeout {
			continue
		}
		delete(targetInfo.pending, pingID)
		timedOut = true
	}
	if timedOut {
		return r.markLost(ctx, out, target, targetInfo)
	}
	return true
}

func (r *Runner) handlePONG(ctx context.Context, out chan<- Event, event Event) bool {
	targetInfo := r.targets[event.Target]
	if targetInfo == nil {
		return true
	}
	timeMS, ok := targetInfo.pending[event.PingID]
	if !ok || timeMS != event.TimeMS {
		return true
	}
	delete(targetInfo.pending, event.PingID)
	targetInfo.lossCount = 0
	if !targetInfo.lost {
		targetInfo.recoverSuccess = 0
		return true
	}

	targetInfo.recoverSuccess++
	if targetInfo.recoverSuccess < r.config.RecoverSuccess {
		return true
	}
	targetInfo.lost = false
	targetInfo.recoverSuccess = 0
	return emit(ctx, out, Event{Type: EventTargetRecovered, Target: event.Target})
}

func (r *Runner) markLost(ctx context.Context, out chan<- Event, target Target, targetInfo *targetRuntime) bool {
	targetInfo.lossCount++
	targetInfo.recoverSuccess = 0
	if targetInfo.lost || targetInfo.lossCount < r.config.MaxLoss {
		return true
	}
	targetInfo.lost = true
	return emit(ctx, out, Event{Type: EventTargetLost, Target: target})
}

func emit(ctx context.Context, out chan<- Event, event Event) bool {
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
