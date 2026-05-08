# Bandwidth Probe Train Protocol Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `BW_PROBE_DONE` with train metadata on `BW_PROBE`, and enforce one global client/server/lane bandwidth-probe sequence.

**Architecture:** `protocol` owns the wire shape and rejects invalid metadata. `recv` only dispatches `BW_PROBE` and `BW_PROBE_ACK`; `send.RecvState` bridges those frames into Send-owned bandwidth state. `send` owns local train budget, remote train receive state, and the session-level bandwidth gate without exporting new control APIs.

**Tech Stack:** Go, existing `internal/protocol`, `internal/tunnel/send`, `internal/tunnel/recv`, existing `golang.org/x/time/rate` token bucket, existing RTT estimators.

---

## File Structure

- Modify `docs/protocol.md`: remove `BW_PROBE_DONE`; document `BW_PROBE` train fields, completion, budget, serialization, and timeout behavior.
- Modify `docs/architecture.md`: remove `OnBandwidthProbeDone` from `recv.ControlState` and control dispatch text.
- Modify `internal/protocol/protocol.go`: remove `TypeBandwidthProbeDone`; make type 9 the maximum valid frame type.
- Modify `internal/protocol/body.go`: extend `BandwidthProbeBody`; encode/decode 44-byte fixed header before payload; remove `BandwidthProbeDoneBody`.
- Modify `internal/protocol/debug.go`: log new `BW_PROBE` fields.
- Modify `internal/protocol/body_test.go` and `internal/protocol/protocol_test.go`: update round trips and invalid-frame coverage.
- Modify `internal/tunnel/recv/recv.go` and `internal/tunnel/recv/debug.go`: remove `BW_PROBE_DONE` from interface, dispatch, handler, and debug names.
- Modify `internal/tunnel/send/module.go`: replace `bandwidthProbeServerReady` with a session-level gate map and remote train state maps; update frame capacity.
- Modify `internal/tunnel/send/bandwidth_probe.go`: add train ID/budget fields, gate helpers, remote train completion and timeout, UDP-only cap behavior, no DONE send.
- Modify `internal/tunnel/send/recv_state.go`, `internal/tunnel/send/debug.go`, and `internal/tunnel/send/send_test.go`: remove DONE bridge and debug/test dispatch.
- Modify `app.go`: remove `EnableBandwidthProbeServerReady`.
- Modify `internal/tunnel/send/bandwidth_probe_test.go`: replace server-ready tests with gate/budget tests and update existing tests for new fields.

## Task 1: Protocol Wire Format

**Files:**
- Modify: `internal/protocol/protocol.go`
- Modify: `internal/protocol/body.go`
- Modify: `internal/protocol/debug.go`
- Test: `internal/protocol/body_test.go`
- Test: `internal/protocol/protocol_test.go`

- [ ] **Step 1: Update protocol tests for the new shape**

In `internal/protocol/body_test.go`, replace the `BandwidthProbeBody` round-trip case with:

```go
{
	Type:      TypeBandwidthProbe,
	SessionID: 11,
	LaneID:    1,
	Body: BandwidthProbeBody{
		TrainID:             77,
		ProbeID:             99,
		Seq:                 2,
		Count:               4,
		SendMS:              12347,
		TrainBytesTotal:     1000,
		TrainBytesRemaining: 250,
		Payload:             []byte("probe"),
	},
},
```

Remove the `TypeBandwidthProbeDone` round-trip case. Remove `TypeBandwidthProbeDone` from the `TestBodyTooShort` frame type list.

Add these tests to `internal/protocol/body_test.go`:

```go
func TestBandwidthProbeRejectsInvalidTrainBudget(t *testing.T) {
	tests := []BandwidthProbeBody{
		{TrainID: 1, ProbeID: 1, Seq: 0, Count: 1, SendMS: 1, TrainBytesTotal: 0, TrainBytesRemaining: 0},
		{TrainID: 1, ProbeID: 1, Seq: 0, Count: 1, SendMS: 1, TrainBytesTotal: 100, TrainBytesRemaining: 101},
	}
	for _, body := range tests {
		_, err := Encode(Frame{Type: TypeBandwidthProbe, SessionID: 1, LaneID: 1, Body: body}, nil)
		if !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("Encode(%+v) err = %v, want ErrInvalidFrame", body, err)
		}
	}
}

func TestBandwidthProbeDecodeRejectsInvalidTrainBudget(t *testing.T) {
	encoded, err := Encode(Frame{
		Type:      TypeBandwidthProbe,
		SessionID: 1,
		LaneID:    1,
		Body: BandwidthProbeBody{
			TrainID:             1,
			ProbeID:             1,
			Seq:                 0,
			Count:               1,
			SendMS:              1,
			TrainBytesTotal:     100,
			TrainBytesRemaining: 100,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode valid probe failed: %v", err)
	}
	binary.BigEndian.PutUint64(encoded[10+36:10+44], 101)
	if _, err := Decode(encoded); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode invalid remaining err = %v, want ErrInvalidFrame", err)
	}
}
```

Add `encoding/binary` to the imports in `body_test.go`.

In `internal/protocol/protocol_test.go`, keep the unknown type fixture at `0x0f`, and change the invalid encode assertion to:

```go
if _, err := Encode(Frame{Type: TypeBandwidthProbeAck + 1}, nil); !errors.Is(err, ErrInvalidFrame) {
	t.Fatalf("Encode invalid type err = %v, want ErrInvalidFrame", err)
}
```

- [ ] **Step 2: Run protocol tests to see the expected failure**

Run:

```bash
go test ./internal/protocol -run 'TestTypedFramesRoundTrip|TestBodyTooShort|TestBandwidthProbe|TestCodecRejectsInvalidFrames' -count=1
```

Expected: FAIL because the code still lacks the new fields and still accepts `TypeBandwidthProbeDone`.

- [ ] **Step 3: Implement protocol changes**

In `internal/protocol/protocol.go`, remove `TypeBandwidthProbeDone` from the `const` block. Change both type-bound checks from `TypeBandwidthProbeDone` to `TypeBandwidthProbeAck`:

```go
if frame.Type == 0 || frame.Type > TypeBandwidthProbeAck {
```

```go
if version != Version || frameType == 0 || frameType > TypeBandwidthProbeAck {
```

In `internal/protocol/body.go`, replace `BandwidthProbeBody` and remove `BandwidthProbeDoneBody`:

```go
type BandwidthProbeBody struct {
	TrainID             uint64
	ProbeID             uint64
	Seq                 uint16
	Count               uint16
	SendMS              uint64
	TrainBytesTotal     uint64
	TrainBytesRemaining uint64
	Payload             []byte
}
```

Update `encodedBodySize` for `TypeBandwidthProbe`:

```go
case TypeBandwidthProbe:
	body, ok := frame.Body.(BandwidthProbeBody)
	if !ok || body.Count == 0 || body.Count > 64 || body.Seq >= body.Count ||
		body.TrainBytesTotal == 0 || body.TrainBytesRemaining > body.TrainBytesTotal {
		return 0, ErrInvalidFrame
	}
	return 44 + len(body.Payload), nil
```

Remove the `TypeBandwidthProbeDone` case from `encodedBodySize`, `encodeBodyInto`, and `decodeBody`.

Update `encodeBodyInto` for `TypeBandwidthProbe`:

```go
case TypeBandwidthProbe:
	body := frame.Body.(BandwidthProbeBody)
	binary.BigEndian.PutUint64(out[:8], body.TrainID)
	binary.BigEndian.PutUint64(out[8:16], body.ProbeID)
	binary.BigEndian.PutUint16(out[16:18], body.Seq)
	binary.BigEndian.PutUint16(out[18:20], body.Count)
	binary.BigEndian.PutUint64(out[20:28], body.SendMS)
	binary.BigEndian.PutUint64(out[28:36], body.TrainBytesTotal)
	binary.BigEndian.PutUint64(out[36:44], body.TrainBytesRemaining)
	copy(out[44:], body.Payload)
```

Update `decodeBody` for `TypeBandwidthProbe`:

```go
case TypeBandwidthProbe:
	if len(body) < 44 {
		return ErrBodyTooShort
	}
	count := binary.BigEndian.Uint16(body[18:20])
	seq := binary.BigEndian.Uint16(body[16:18])
	total := binary.BigEndian.Uint64(body[28:36])
	remaining := binary.BigEndian.Uint64(body[36:44])
	if count == 0 || count > 64 || seq >= count || total == 0 || remaining > total {
		return ErrInvalidFrame
	}
	frame.Body = BandwidthProbeBody{
		TrainID:             binary.BigEndian.Uint64(body[:8]),
		ProbeID:             binary.BigEndian.Uint64(body[8:16]),
		Seq:                 seq,
		Count:               count,
		SendMS:              binary.BigEndian.Uint64(body[20:28]),
		TrainBytesTotal:     total,
		TrainBytesRemaining: remaining,
		Payload:             body[44:],
	}
```

In `internal/protocol/debug.go`, update the `BandwidthProbeBody` debug string:

```go
return fmt.Sprintf("%s train_id=%d probe_id=%d seq=%d count=%d send_ms=%d train_total=%d train_remaining=%d payload_len=%d", base, body.TrainID, body.ProbeID, body.Seq, body.Count, body.SendMS, body.TrainBytesTotal, body.TrainBytesRemaining, len(body.Payload))
```

- [ ] **Step 4: Run protocol tests**

Run:

```bash
gofmt -w internal/protocol/protocol.go internal/protocol/body.go internal/protocol/debug.go internal/protocol/body_test.go internal/protocol/protocol_test.go
go test ./internal/protocol -count=1
```

Expected: PASS.

## Task 2: Remove DONE From Recv and Control Bridge

**Files:**
- Modify: `internal/tunnel/recv/recv.go`
- Modify: `internal/tunnel/recv/debug.go`
- Modify: `internal/tunnel/send/recv_state.go`
- Modify: `internal/tunnel/send/debug.go`
- Modify: `internal/tunnel/send/send_test.go`
- Modify: `internal/tunnel/send/module.go`
- Modify: `app.go`

- [ ] **Step 1: Remove DONE dispatch tests by compile**

Run:

```bash
go test ./internal/tunnel/recv ./internal/tunnel/send -run TestDoesNotExist -count=1
```

Expected: FAIL after Task 1 until references to `TypeBandwidthProbeDone` and `BandwidthProbeDoneBody` are removed.

- [ ] **Step 2: Remove DONE from recv**

In `internal/tunnel/recv/recv.go`, remove `OnBandwidthProbeDone` from `ControlState`, remove the `case protocol.TypeBandwidthProbeDone` dispatch, and delete `handleBandwidthProbeDone`.

In `internal/tunnel/recv/debug.go`, remove the `case protocol.TypeBandwidthProbeDone` branch from `debugFrameType`.

- [ ] **Step 3: Remove DONE from send bridge and debug**

In `internal/tunnel/send/recv_state.go`, delete `OnBandwidthProbeDone`.

In `internal/tunnel/send/debug.go`, update the `BandwidthProbeBody` debug string with train fields, and remove the `TypeBandwidthProbeDone` branch from `debugFrameType`.

In `internal/tunnel/send/send_test.go`, remove the `TypeBandwidthProbeDone` case from the test dispatch helper near the bottom of the file.

In `internal/tunnel/send/module.go`, update `frameEncodeCapacity` for `TypeBandwidthProbe` to return `headerSize + 44 + len(body.Payload)` and validate `TrainBytesTotal`/`TrainBytesRemaining`; delete the `TypeBandwidthProbeDone` case.

In `app.go`, remove:

```go
in.EnableBandwidthProbeServerReady()
```

- [ ] **Step 4: Run recv/send compile checks**

Run:

```bash
gofmt -w internal/tunnel/recv/recv.go internal/tunnel/recv/debug.go internal/tunnel/send/recv_state.go internal/tunnel/send/debug.go internal/tunnel/send/send_test.go internal/tunnel/send/module.go app.go
go test ./internal/tunnel/recv ./internal/tunnel/send -run TestDoesNotExist -count=1
```

Expected: PASS compile with no tests run.

## Task 3: Add Local Train Budget Metadata

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Test: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Add failing budget tests**

In `internal/tunnel/send/bandwidth_probe_test.go`, add:

```go
func TestBandwidthProbeTrainBudgetFromCap(t *testing.T) {
	budget := bandwidthProbeTrainBudgetBytes(200_000_000)
	want := uint64(200_000_000) * uint64(bandwidthProbeWindow) / uint64(time.Second) / 8
	if budget != want {
		t.Fatalf("budget = %d, want %d", budget, want)
	}
}

func TestBandwidthProbeRoundCarriesTrainBudget(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	leg := udpLeg()
	legKey := newPingKey(leg)
	budget := uint64(10_000)

	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:                 key,
		rateBps:             bandwidthProbeMinRateBps,
		capBps:              bandwidthProbeMinRateBps,
		inFlight:            true,
		trainID:             42,
		trainBytesTotal:     budget,
		trainBytesRemaining: budget,
		steps:               make(map[uint64]*bandwidthProbeStep),
	}

	round := in.startBandwidthProbeRound(key, leg, legKey, 1, time.Now())
	if round == nil {
		t.Fatal("nil round")
	}
	if round.trainID != 42 || round.trainBytesTotal != budget || round.trainBytesRemaining != budget {
		t.Fatalf("round train fields = id=%d total=%d remaining=%d, want 42/%d/%d", round.trainID, round.trainBytesTotal, round.trainBytesRemaining, budget, budget)
	}
}

func TestBandwidthProbeConsumeTrainBudget(t *testing.T) {
	remaining, last := bandwidthProbeConsumeBudget(1000, 300)
	if remaining != 700 || last {
		t.Fatalf("first consume = (%d,%t), want (700,false)", remaining, last)
	}
	remaining, last = bandwidthProbeConsumeBudget(200, 300)
	if remaining != 0 || !last {
		t.Fatalf("last consume = (%d,%t), want (0,true)", remaining, last)
	}
}
```

- [ ] **Step 2: Run budget tests to see failure**

Run:

```bash
go test ./internal/tunnel/send -run 'TestBandwidthProbeTrainBudgetFromCap|TestBandwidthProbeRoundCarriesTrainBudget|TestBandwidthProbeConsumeTrainBudget' -count=1
```

Expected: FAIL because helpers and fields do not exist.

- [ ] **Step 3: Implement train budget fields**

In `internal/tunnel/send/bandwidth_probe.go`, add to `bandwidthLegState`:

```go
trainID             uint64
trainBytesTotal     uint64
trainBytesRemaining uint64
```

Add to `bandwidthProbeRound`:

```go
trainID             uint64
trainBytesTotal     uint64
trainBytesRemaining uint64
```

Add helpers:

```go
func bandwidthProbeTrainBudgetBytes(rateBps uint64) uint64 {
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	return rateBps * uint64(bandwidthProbeWindow) / uint64(time.Second) / 8
}

func bandwidthProbeConsumeBudget(remaining uint64, frameBytes int) (uint64, bool) {
	if frameBytes <= 0 {
		return remaining, remaining == 0
	}
	frame := uint64(frameBytes)
	if frame >= remaining {
		return 0, true
	}
	return remaining - frame, false
}
```

In `maybeStartBandwidthProbe`, after computing `state.capBps`, compute the effective budget rate:

```go
budgetRate := state.capBps
if budgetRate == 0 {
	budgetRate = bandwidthProbeTCPRateBps
}
if leg.Kind == transport.KindUDP {
	if ref := l.bandwidthProbeTCPReferenceBps(key); state.capBps == 0 && ref > 0 {
		budgetRate = ref
	}
}
state.trainID = l.nextBWProbeID.Add(1)
state.trainBytesTotal = bandwidthProbeTrainBudgetBytes(budgetRate)
state.trainBytesRemaining = state.trainBytesTotal
```

In `startBandwidthProbeRound`, copy the train fields from state onto the round.

In `runBandwidthProbeRound`, before creating the frame body, consume the budget and include the train fields:

```go
remaining, last := bandwidthProbeConsumeBudget(round.trainBytesRemaining, round.frameBytes)
round.trainBytesRemaining = remaining
frame := protocol.Frame{
	Type:      protocol.TypeBandwidthProbe,
	SessionID: round.key.sessionID,
	LaneID:    round.key.laneID,
	Body: protocol.BandwidthProbeBody{
		TrainID:             round.trainID,
		ProbeID:             round.probeID,
		Seq:                 seq,
		Count:               round.count,
		SendMS:              nowMS,
		TrainBytesTotal:     round.trainBytesTotal,
		TrainBytesRemaining: remaining,
		Payload:             payload,
	},
}
```

After a successful send, update the state copy under `bandwidthMu`:

```go
l.updateBandwidthProbeTrainRemaining(round.legKey, remaining)
if last {
	return seq + 1
}
```

Add:

```go
func (l *Send) updateBandwidthProbeTrainRemaining(legKey pingKey, remaining uint64) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && state.inFlight && !state.complete {
		state.trainBytesRemaining = remaining
	}
	l.bandwidthMu.Unlock()
}
```

In `runBandwidthProbeTrain`, stop the outer loops when `trainBytesRemaining == 0` by adding:

```go
if l.bandwidthProbeTrainBudgetDepleted(legKey) {
	break
}
```

inside both loops after each round. Add:

```go
func (l *Send) bandwidthProbeTrainBudgetDepleted(legKey pingKey) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	state := l.bandwidthLegs[legKey]
	return state != nil && state.inFlight && state.trainBytesTotal > 0 && state.trainBytesRemaining == 0
}
```

- [ ] **Step 4: Run budget tests**

Run:

```bash
gofmt -w internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/bandwidth_probe_test.go
go test ./internal/tunnel/send -run 'TestBandwidthProbeTrainBudgetFromCap|TestBandwidthProbeRoundCarriesTrainBudget|TestBandwidthProbeConsumeTrainBudget|TestBandwidthProbeFrameEncoding' -count=1
```

Expected: PASS.

## Task 4: Replace Server-Ready Map With Session Gate

**Files:**
- Modify: `internal/tunnel/send/module.go`
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Test: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Replace old gate tests**

Delete `TestBandwidthProbeServerReadyNil` and `TestBandwidthProbeServerReadyMarked`.

Add:

```go
func TestBandwidthProbeGateClientWaitsForRemoteBeforeNextLane(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	key1 := laneKey{sessionID: 99, laneID: 1}
	key2 := laneKey{sessionID: 99, laneID: 2}

	in.advanceBandwidthProbeGateAfterLocal(key1, transport.KindUDP)
	if in.bandwidthProbeGateAllowsLocal(key2, transport.KindUDP) {
		t.Fatal("client gate allowed lane2 before remote lane1 completion")
	}
	if !in.bandwidthProbeGateAllowsRemote(key1, transport.KindUDP) {
		t.Fatal("client gate should wait for remote lane1")
	}
}

func TestBandwidthProbeGateAdvancesAfterRemoteCompletion(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.bandwidthProbeCapBps = bandwidthProbeMinRateBps
	key1 := laneKey{sessionID: 99, laneID: 1}
	key2 := laneKey{sessionID: 99, laneID: 2}

	in.advanceBandwidthProbeGateAfterLocal(key1, transport.KindUDP)
	in.advanceBandwidthProbeGateAfterRemote(key1, transport.KindUDP)
	if !in.bandwidthProbeGateAllowsLocal(key2, transport.KindUDP) {
		t.Fatal("client gate did not advance to local lane2")
	}
}

func TestBandwidthProbeGateServerStartsRemotePhase(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.setBandwidthProbeGateMode(false)
	key1 := laneKey{sessionID: 99, laneID: 1}
	if in.bandwidthProbeGateAllowsLocal(key1, transport.KindUDP) {
		t.Fatal("server should not start local train before client train")
	}
	if !in.bandwidthProbeGateAllowsRemote(key1, transport.KindUDP) {
		t.Fatal("server should accept remote lane1 train first")
	}
}
```

- [ ] **Step 2: Run gate tests to see failure**

Run:

```bash
go test ./internal/tunnel/send -run 'TestBandwidthProbeGate' -count=1
```

Expected: FAIL because gate helpers do not exist and old server-ready functions were removed.

- [ ] **Step 3: Add gate state**

In `internal/tunnel/send/module.go`, remove:

```go
bandwidthProbeServerReady map[laneKey]bool
```

Add:

```go
bandwidthGates map[uint64]bandwidthProbeGate
```

Initialize it in `New`:

```go
bandwidthGates: make(map[uint64]bandwidthProbeGate),
```

In `internal/tunnel/send/bandwidth_probe.go`, add:

```go
type bandwidthProbePhase uint8

const (
	bandwidthProbePhaseLocal bandwidthProbePhase = iota
	bandwidthProbePhaseRemote
)

type bandwidthProbeGate struct {
	sessionID uint64
	laneID    uint8
	legKind   transport.Kind
	phase     bandwidthProbePhase
	localFirst bool
}
```

Add helpers:

```go
func (l *Send) setBandwidthProbeGateMode(localFirst bool) {
	sessionID, ok := l.activeSession()
	if !ok {
		return
	}
	l.bandwidthMu.Lock()
	gate := l.bandwidthProbeGateLocked(sessionID)
	gate.localFirst = localFirst
	if localFirst {
		gate.phase = bandwidthProbePhaseLocal
	} else {
		gate.phase = bandwidthProbePhaseRemote
	}
	l.bandwidthGates[sessionID] = gate
	l.bandwidthMu.Unlock()
}

func (l *Send) bandwidthProbeGateLocked(sessionID uint64) bandwidthProbeGate {
	if gate, ok := l.bandwidthGates[sessionID]; ok {
		return gate
	}
	return bandwidthProbeGate{
		sessionID:  sessionID,
		laneID:     1,
		legKind:    l.firstBandwidthProbeLegKind(),
		phase:      bandwidthProbePhaseLocal,
		localFirst: true,
	}
}

func (l *Send) firstBandwidthProbeLegKind() transport.Kind {
	if l.bandwidthProbeCapBps > 0 {
		return transport.KindUDP
	}
	return transport.KindTCP
}
```

Add `bandwidthProbeGateAllowsLocal`, `bandwidthProbeGateAllowsRemote`, `advanceBandwidthProbeGateAfterLocal`, and `advanceBandwidthProbeGateAfterRemote`. The advance rules:

- Local completion switches phase to remote for the same lane/leg.
- Remote completion of TCP with no cap switches to local UDP same lane.
- Remote completion of UDP switches to local next lane UDP when capped.
- Remote completion of UDP switches to local next lane TCP when uncapped.
- On server (`localFirst=false`), remote completion switches to local same lane/leg; local completion advances to remote next leg/lane.

Use a helper:

```go
func (l *Send) nextBandwidthProbeGateAfterPair(gate bandwidthProbeGate) bandwidthProbeGate {
	if l.bandwidthProbeCapBps == 0 && gate.legKind == transport.KindTCP {
		gate.legKind = transport.KindUDP
		return gate
	}
	gate.laneID++
	gate.legKind = l.firstBandwidthProbeLegKind()
	return gate
}
```

- [ ] **Step 4: Wire gate into probe selection**

In `probeBandwidth`, remove the `isBandwidthProbeServerReady` check and the `activeBandwidthLane` early return. Keep the active-in-flight check, but make it block only when an actual in-flight state exists. Filter candidates with:

```go
if !l.bandwidthProbeGateAllowsLocal(item.key, leg.Kind) {
	continue
}
```

When the local train completes in `completeBandwidthProbeTrain`, call:

```go
l.advanceBandwidthProbeGateAfterLocal(key, leg.Kind)
```

When a local train is lost in `completeBandwidthProbeLostLeg`, call the same after settling so the gate does not stall forever.

Delete `EnableBandwidthProbeServerReady`, `isBandwidthProbeServerReady`, and `markBandwidthProbeDone`.

- [ ] **Step 5: Run gate tests**

Run:

```bash
gofmt -w internal/tunnel/send/module.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/bandwidth_probe_test.go
go test ./internal/tunnel/send -run 'TestBandwidthProbeGate|TestBandwidthProbeTrainDoesNotCompleteAfterLostLeg' -count=1
```

Expected: PASS.

## Task 5: Remote Train Completion and Timeout

**Files:**
- Modify: `internal/tunnel/send/module.go`
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Test: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Add remote completion tests**

Add to `internal/tunnel/send/bandwidth_probe_test.go`:

```go
func TestReceiveBandwidthProbeRemainingZeroAdvancesGate(t *testing.T) {
	in := New()
	in.activateSession(99)
	in.setBandwidthProbeGateMode(false)
	key := laneKey{sessionID: 99, laneID: 1}
	leg := udpLeg()
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(leg)
	in.lanes[key] = lane

	body := protocol.BandwidthProbeBody{
		TrainID:             7,
		ProbeID:             8,
		Seq:                 0,
		Count:               1,
		SendMS:              1,
		TrainBytesTotal:     100,
		TrainBytesRemaining: 0,
		Payload:             []byte("x"),
	}
	if err := in.receiveBandwidthProbe(context.Background(), 99, 1, leg, body); err != nil {
		t.Fatalf("receiveBandwidthProbe failed: %v", err)
	}
	if !in.bandwidthProbeGateAllowsLocal(key, transport.KindUDP) {
		t.Fatal("server gate did not advance to local after remote completion")
	}
}

func TestRemoteTrainIdleTimeoutDoesNotMarkLaneDown(t *testing.T) {
	in := New(send.Config{ProbeTimeout: time.Second})
	key := laneKey{sessionID: 99, laneID: 1}
	leg := udpLeg()
	lane := newLaneRuntime(1, 1)
	lane.bindLeg(leg)
	in.lanes[key] = lane

	timeout := in.remoteBandwidthTrainIdleTimeout(key, leg)
	if timeout <= 0 {
		t.Fatalf("timeout = %s, want positive", timeout)
	}
	in.completeRemoteBandwidthProbeTrain(key, leg, 7, "idle_timeout")
	udpLeg, udpQ, _, _ := lane.legQualities()
	if !udpQ.Active || newPingKey(udpLeg) != newPingKey(leg) {
		t.Fatal("remote train timeout should not mark UDP leg down")
	}
}
```

Add `protocol` to imports if not present.

- [ ] **Step 2: Run remote tests to see failure**

Run:

```bash
go test ./internal/tunnel/send -run 'TestReceiveBandwidthProbeRemainingZeroAdvancesGate|TestRemoteTrainIdleTimeoutDoesNotMarkLaneDown' -count=1
```

Expected: FAIL because remote train state does not exist.

- [ ] **Step 3: Add remote train state and validation**

In `internal/tunnel/send/module.go`, add:

```go
bandwidthRemoteTrains map[bandwidthRemoteTrainKey]*bandwidthRemoteTrain
```

Initialize it in `New`.

In `internal/tunnel/send/bandwidth_probe.go`, add:

```go
type bandwidthRemoteTrainKey struct {
	legKey  pingKey
	trainID uint64
}

type bandwidthRemoteTrain struct {
	key        laneKey
	leg        transport.LegRef
	trainID    uint64
	totalBytes uint64
	firstRXMS  uint64
	lastRXMS   uint64
	timer      *time.Timer
}
```

In `receiveBandwidthProbe`, validate that `body.TrainBytesTotal != 0`, `body.TrainBytesRemaining <= body.TrainBytesTotal`, and the gate allows remote for `(sessionID,laneID,leg.Kind)`. Keep stale/future trains logged and dropped without advancing the gate.

Track the remote train under `bandwidthMu`. If the same `TrainID` changes `TrainBytesTotal`, drop it. Reset its idle timer on each frame.

Add:

```go
func (l *Send) remoteBandwidthTrainIdleTimeout(key laneKey, leg transport.LegRef) time.Duration {
	lane := l.getLane(key)
	if lane != nil {
		lane.mu.Lock()
		srttMS, ok := laneSRTTLocked(lane, leg.Kind)
		lane.mu.Unlock()
		if ok && srttMS > 0 {
			timeout := time.Duration(srttMS) * time.Millisecond * 8
			if timeout < 500*time.Millisecond {
				return 500 * time.Millisecond
			}
			if timeout > 10*time.Second {
				return 10 * time.Second
			}
			return timeout
		}
	}
	if l.probeTimeout > 0 {
		return l.probeTimeout
	}
	return 10 * time.Second
}
```

Add `armRemoteBandwidthTrainTimer` that uses `time.AfterFunc(timeout, func() { l.completeRemoteBandwidthProbeTrain(key, leg, trainID, "idle_timeout") })`.

Add `completeRemoteBandwidthProbeTrain` that stops and removes the remote train and all `bandwidthRX` rounds with the same `legKey`, then calls `advanceBandwidthProbeGateAfterRemote(key, leg.Kind)`. This function must not call `markUDPNotReady`, `markTCPNotReady`, `handleProbeTargetLost`, or log lane down.

In `receiveBandwidthProbe`, after ACK state update and before returning, if `body.TrainBytesRemaining == 0`, call:

```go
l.completeRemoteBandwidthProbeTrain(laneKey{sessionID: sessionID, laneID: laneID}, leg, body.TrainID, "remaining_zero")
```

- [ ] **Step 4: Run remote tests**

Run:

```bash
gofmt -w internal/tunnel/send/module.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/bandwidth_probe_test.go
go test ./internal/tunnel/send -run 'TestReceiveBandwidthProbeRemainingZeroAdvancesGate|TestRemoteTrainIdleTimeoutDoesNotMarkLaneDown|TestBandwidthProbeGate' -count=1
```

Expected: PASS.

## Task 6: Cap Rules and Candidate Ordering

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Test: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Add cap behavior tests**

Add:

```go
func TestBandwidthProbeCandidateWithCapSkipsTCP(t *testing.T) {
	in := New()
	in.bandwidthProbeCapBps = 200_000_000
	key := laneKey{sessionID: 99, laneID: 1}
	lane := newLaneRuntime(1, 1)
	udp := udpLeg()
	tcp := tcpLeg()
	lane.bindLeg(udp)
	lane.bindLeg(tcp)

	leg, ok := in.bandwidthProbeCandidate(key, lane, udp, LegQuality{Active: true}, tcp, LegQuality{Active: true})
	if !ok || leg.Kind != transport.KindUDP {
		t.Fatalf("candidate = (%s,%t), want UDP", debugLeg(leg), ok)
	}
}

func TestBandwidthProbeCandidateNoCapRunsTCPReferenceFirst(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	lane := newLaneRuntime(1, 1)
	udp := udpLeg()
	tcp := tcpLeg()
	lane.bindLeg(udp)
	lane.bindLeg(tcp)

	leg, ok := in.bandwidthProbeCandidate(key, lane, udp, LegQuality{Active: true}, tcp, LegQuality{Active: true})
	if !ok || leg.Kind != transport.KindTCP {
		t.Fatalf("candidate = (%s,%t), want TCP reference", debugLeg(leg), ok)
	}
}
```

- [ ] **Step 2: Run cap tests to see current behavior**

Run:

```bash
go test ./internal/tunnel/send -run 'TestBandwidthProbeCandidateWithCapSkipsTCP|TestBandwidthProbeCandidateNoCapRunsTCPReferenceFirst' -count=1
```

Expected: at least the capped test FAILS because current code prefers missing TCP samples even with a cap.

- [ ] **Step 3: Implement cap rules**

In `bandwidthProbeCandidate`, add the cap branch first:

```go
if l.bandwidthProbeCapBps > 0 {
	if l.bandwidthProbeNeeded(udpLeg, udpQ) {
		return udpLeg, true
	}
	return transport.LegRef{}, false
}
```

Then keep the existing uncapped TCP-reference-first behavior for `capBps == 0`.

Update `logBandwidthProbeDecisionIfReady` so capped UDP-only probing can log/settle without requiring a TCP sample. For capped mode, return after logging a UDP-only decision or leave the existing decision log quiet if no TCP exists; do not block gate progression on this.

- [ ] **Step 4: Run cap tests**

Run:

```bash
gofmt -w internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/bandwidth_probe_test.go
go test ./internal/tunnel/send -run 'TestBandwidthProbeCandidate|TestBandwidthProbeGate|TestBandwidthProbeTrainBudget' -count=1
```

Expected: PASS.

## Task 7: Update Docs

**Files:**
- Modify: `docs/protocol.md`
- Modify: `docs/architecture.md`

- [ ] **Step 1: Update architecture control interface text**

In `docs/architecture.md`, remove `OnBandwidthProbeDone` from the `ControlState` sketch and ensure the dispatch text says:

```text
Recv passes decoded HELLO, HELLO_ACK, PING, PONG, CLOSE, BW_PROBE, and
BW_PROBE_ACK frames to the configured ControlState.
```

- [ ] **Step 2: Update protocol BW_PROBE section**

In `docs/protocol.md`, replace the `BW_PROBE` body block with:

```text
train_id              uint64
probe_id              uint64
seq                   uint16
count                 uint16 // total frames in this probe round, 1..64
send_ms               uint64
train_bytes_total     uint64
train_bytes_remaining uint64
payload               bytes
```

Add sender/receiver rules:

```text
`train_bytes_total` is the byte budget for one train. It must be non-zero.
`train_bytes_remaining` is the byte budget remaining after this frame and must
not exceed `train_bytes_total`. A value of zero marks normal train completion.
The same `train_id` must not change `train_bytes_total`.
```

Add a serialization paragraph:

```text
Bandwidth trains are globally serialized per session. The order is client lane
N, server lane N, client lane N+1, server lane N+1. When no configured cap is
present, each lane first runs TCP as a reference and then UDP; when a cap is
present, only UDP is probed.
```

Add timeout behavior:

```text
If the last train frame is lost, the receiver releases only the bandwidth
probe gate after an idle timeout of clamp(8*SRTT, 500ms, 10s), falling back to
the configured probe timeout when SRTT is unavailable. This timeout does not
mark the lane or leg down.
```

Remove any references to `BW_PROBE_DONE`.

- [ ] **Step 3: Check docs for stale DONE text**

Run:

```bash
rg -n "BW_PROBE_DONE|BandwidthProbeDone|OnBandwidthProbeDone|EnableBandwidthProbeServerReady|bandwidthProbeServerReady" docs internal app.go
```

Expected: only historical files under `docs/superpowers/specs` or old plan files may mention the removed names; source files and `docs/protocol.md`/`docs/architecture.md` must not.

## Task 8: Full Verification and Commit

**Files:**
- All modified files

- [ ] **Step 1: Run targeted tests**

Run:

```bash
go test ./internal/protocol ./internal/tunnel/recv ./internal/tunnel/send -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full build**

Run:

```bash
go build ./...
```

Expected: PASS.

- [ ] **Step 3: Inspect diff**

Run:

```bash
git diff --stat
git diff -- docs/protocol.md docs/architecture.md internal/protocol/protocol.go internal/protocol/body.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/recv/recv.go
```

Expected: diff removes DONE, adds train metadata, and does not add new exported Send control APIs.

- [ ] **Step 4: Commit**

Run:

```bash
git add docs/protocol.md docs/architecture.md app.go internal/protocol internal/tunnel/recv internal/tunnel/send
git commit -m "fix: serialize bandwidth probe trains"
```

Expected: a new commit after `0d7b91e`; do not amend.

## Self-Review

- Spec coverage: protocol metadata, removal of DONE, train budget, cap rules, client/server/lane serialization, remote completion, idle timeout, and docs are all covered.
- Placeholder scan: no `TBD`, `TODO`, or vague "add tests" steps remain.
- Type consistency: plan consistently uses `BandwidthProbeBody.TrainID`, `TrainBytesTotal`, `TrainBytesRemaining`, `bandwidthProbeGate`, `bandwidthRemoteTrain`, and existing unexported Send methods only.
