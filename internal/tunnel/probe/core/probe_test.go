package core

import (
	"context"
	"testing"
	"time"
)

func TestRunnerTickEmitsPING(t *testing.T) {
	runner := New(Config{Timeout: time.Second})
	out := make(chan Event, 4)
	ctx := context.Background()

	if !runner.handleInput(ctx, out, Event{Type: EventTrack, Target: 1}) {
		t.Fatal("handleInput returned false")
	}
	if !runner.tick(ctx, out, 1000) {
		t.Fatal("tick returned false")
	}

	event := readProbeEvent(t, out)
	if event.Type != EventSendPing || event.Target != 1 || event.PingID != 0 || event.TimeMS != 1000 {
		t.Fatalf("event = %+v, want EventSendPing target=1 pingID=0 timeMS=1000", event)
	}
}

func TestRunnerTimeoutAndRecovery(t *testing.T) {
	runner := New(Config{
		Timeout:        time.Millisecond,
		MaxLoss:        1,
		RecoverSuccess: 1,
	})
	out := make(chan Event, 8)
	ctx := context.Background()

	if !runner.handleInput(ctx, out, Event{Type: EventTrack, Target: 1}) {
		t.Fatal("handleInput returned false")
	}
	if !runner.tick(ctx, out, 1000) {
		t.Fatal("first tick returned false")
	}
	firstPING := readProbeEvent(t, out)
	if firstPING.Type != EventSendPing {
		t.Fatalf("first event = %+v, want EventSendPing", firstPING)
	}

	if !runner.tick(ctx, out, 1002) {
		t.Fatal("timeout tick returned false")
	}
	lost := readProbeEvent(t, out)
	if lost.Type != EventTargetLost || lost.Target != 1 {
		t.Fatalf("lost event = %+v, want EventTargetLost target=1", lost)
	}
	secondPING := readProbeEvent(t, out)
	if secondPING.Type != EventSendPing || secondPING.Target != 1 {
		t.Fatalf("second PING = %+v, want EventSendPing target=1", secondPING)
	}

	if !runner.handleInput(ctx, out, Event{
		Type:   EventPongReceived,
		Target: 1,
		PingID: secondPING.PingID,
		TimeMS: secondPING.TimeMS,
	}) {
		t.Fatal("PONG input returned false")
	}
	recovered := readProbeEvent(t, out)
	if recovered.Type != EventTargetRecovered || recovered.Target != 1 {
		t.Fatalf("recovered event = %+v, want EventTargetRecovered target=1", recovered)
	}
}

func readProbeEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	default:
		t.Fatal("missing probe event")
		return Event{}
	}
}
