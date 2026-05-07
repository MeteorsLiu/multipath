# Async Bandwidth Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Server waits for client probe completion (per-lane) before starting its own probe, avoiding simultaneous bidirectional probe traffic.

**Architecture:** Client sends `BANDWIDTH_PROBE_DONE` protocol frame on probe completion. Server Recv receives it → RecvState delegates to Send → Send marks lane as server-ready. ProbeBandwidth skips lanes not yet marked ready. Client-side is unaffected (ready map is nil, always passes).

**Tech Stack:** Go, `encoding/binary`, internal protocol/transport/send/recv packages.

---

### Task 1: Protocol — new frame type and body

**Files:**
- Modify: `internal/protocol/protocol.go`
- Modify: `internal/protocol/body.go`
- Modify: `internal/protocol/body_test.go`

- [ ] **Step 1: Add `TypeBandwidthProbeDone` constant**

In `internal/protocol/protocol.go`, add after `TypeBandwidthProbeAck` (line 27):

```go
const (
	TypeHELLO FrameType = iota + 1
	TypeHELLOACK
	TypePING
	TypePONG
	TypeDATA
	TypeREPAIR
	TypeCLOSE
	TypeBandwidthProbe
	TypeBandwidthProbeAck
	TypeBandwidthProbeDone
)
```

- [ ] **Step 2: Add `BandwidthProbeDoneBody` struct**

In `internal/protocol/body.go`, add after `BandwidthProbeAckBody` (near line 98):

```go
type BandwidthProbeDoneBody struct {
	ResultBps uint64
}

func (BandwidthProbeDoneBody) protocolBody() {}
```

- [ ] **Step 3: Add encode/decode cases**

In `internal/protocol/body.go`, add to `encodedBodySize` switch (after the `TypeBandwidthProbeAck` case):

```go
case TypeBandwidthProbeDone:
	_, ok := frame.Body.(BandwidthProbeDoneBody)
	return 8, validBody(ok)
```

In `encodeBodyInto` switch:

```go
case TypeBandwidthProbeDone:
	body := frame.Body.(BandwidthProbeDoneBody)
	binary.BigEndian.PutUint64(out[:8], body.ResultBps)
```

In `decodeBody` switch:

```go
case TypeBandwidthProbeDone:
	if len(body) < 8 {
		return ErrBodyTooShort
	}
	frame.Body = BandwidthProbeDoneBody{
		ResultBps: binary.BigEndian.Uint64(body[:8]),
	}
```

- [ ] **Step 4: Update FrameType bound in `protocol.go`**

In `protocol.go` Encode and Decode, change `TypeBandwidthProbeAck` to `TypeBandwidthProbeDone`:

```go
// In Encode (line 52):
if frame.Type == 0 || frame.Type > TypeBandwidthProbeDone {

// In Decode (line 90):
if frame.Type == 0 || frame.Type > TypeBandwidthProbeDone {
```

- [ ] **Step 5: Add protocol roundtrip test**

In `internal/protocol/body_test.go`, add:

```go
func TestBandwidthProbeDoneBodyRoundtrip(t *testing.T) {
	body := BandwidthProbeDoneBody{ResultBps: 123456789}
	f := Frame{Type: TypeBandwidthProbeDone, SessionID: 42, LaneID: 3, Body: body}
	packet, err := Encode(f, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(packet)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != TypeBandwidthProbeDone {
		t.Fatalf("type = %v", got.Type)
	}
	done, ok := got.Body.(BandwidthProbeDoneBody)
	if !ok {
		t.Fatalf("body type = %T", got.Body)
	}
	if done.ResultBps != 123456789 {
		t.Fatalf("ResultBps = %d", done.ResultBps)
	}
}
```

- [ ] **Step 6: Build and test**

```bash
go build ./internal/protocol/
go test ./internal/protocol/ -run TestBandwidthProbeDoneBodyRoundtrip -v -count=1
```

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/protocol/
git commit -m "protocol: add TypeBandwidthProbeDone frame"
```

---

### Task 2: Send — send DONE frame on probe completion

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Add `sendBandwidthProbeDone` method**

Insert after `completeBandwidthProbeTrain`:

```go
func (l *Send) sendBandwidthProbeDone(key laneKey, leg transport.LegRef, bestBps uint64) {
	frame := protocol.Frame{
		Type:      protocol.TypeBandwidthProbeDone,
		SessionID: key.sessionID,
		LaneID:    key.laneID,
		Body: protocol.BandwidthProbeDoneBody{
			ResultBps: bestBps,
		},
	}
	if err := l.writeControlFrameOnLeg(context.TODO(), leg, frame); err != nil {
		debuglog.Printf("send/bw_probe", "done_send_fail session=%d lane=%d err=%v", key.sessionID, key.laneID, err)
	} else {
		debuglog.Printf("send/bw_probe", "done_sent session=%d lane=%d bps=%d", key.sessionID, key.laneID, bestBps)
	}
}
```

- [ ] **Step 2: Call `sendBandwidthProbeDone` from `completeBandwidthProbeTrain`**

In `completeBandwidthProbeTrain`, after the debug log at the end (after `debuglog.Printf("send/bw_probe", "train_finish"...)`), add:

```go
l.sendBandwidthProbeDone(key, leg, bestBps)
```

- [ ] **Step 3: Build check**

```bash
go build ./internal/tunnel/send/
```

Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "send: send BANDWIDTH_PROBE_DONE on probe completion"
```

---

### Task 3: Send — server-ready lane tracking

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Modify: `internal/tunnel/send/module.go`

- [ ] **Step 1: Add `bandwidthProbeServerReady` to Send struct**

In `internal/tunnel/send/module.go`, find the `Send` struct definition (line ~26) and add the field:

```go
type Send struct {
	// ... existing fields ...
	bandwidthProbeServerReady map[laneKey]bool
}
```

- [ ] **Step 2: Add `isBandwidthProbeServerReady` method**

In `internal/tunnel/send/bandwidth_probe.go`, add:

```go
func (l *Send) isBandwidthProbeServerReady(key laneKey) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	if l.bandwidthProbeServerReady == nil {
		return true
	}
	return l.bandwidthProbeServerReady[key]
}
```

- [ ] **Step 3: Add `markBandwidthProbeDone` method**

In `internal/tunnel/send/bandwidth_probe.go`, add:

```go
func (l *Send) markBandwidthProbeDone(sessionID uint64, laneID uint8) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	if l.bandwidthProbeServerReady == nil {
		l.bandwidthProbeServerReady = make(map[laneKey]bool)
	}
	l.bandwidthProbeServerReady[laneKey{sessionID: sessionID, laneID: laneID}] = true
}
```

- [ ] **Step 4: Add server-ready check to `probeBandwidth` lane loop**

In `probeBandwidth`, in the lane iteration loop (where `bandwidthProbeCandidate` is called), add the check. Find the loop around line 179-184 and add:

```go
for _, item := range lanes {
	if !l.isBandwidthProbeServerReady(item.key) {
		continue
	}
	udpLeg, udpQ, tcpLeg, tcpQ := item.lane.legQualities()
	if _, ok := l.bandwidthProbeCandidate(item.key, item.lane, udpLeg, udpQ, tcpLeg, tcpQ); ok {
		selectedLane = item.key
		break
	}
}
```

- [ ] **Step 5: Build check**

```bash
go build ./internal/tunnel/send/
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/module.go
git commit -m "send: add server-ready lane tracking for async probe"
```

---

### Task 4: Recv — receive DONE and notify Send

**Files:**
- Modify: `internal/tunnel/recv/recv.go`
- Modify: `internal/tunnel/send/recv_state.go`
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Add `OnBandwidthProbeDone` to `ControlState` interface**

In `internal/tunnel/recv/recv.go`, add to the `ControlState` interface (after `OnBandwidthProbeAck`):

```go
type ControlState interface {
	OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
	OnBandwidthProbeDone(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
}
```

- [ ] **Step 2: Add `handleBandwidthProbeDone` handler and dispatch case**

In `internal/tunnel/recv/recv.go`, add handler:

```go
func (o *Recv) handleBandwidthProbeDone(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if _, ok := frame.Body.(protocol.BandwidthProbeDoneBody); !ok {
		debuglog.Printf("recv/control", "bw_probe_done_invalid_body session=%d lane=%d", frame.SessionID, frame.LaneID)
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("recv/control", "bw_probe_done session=%d lane=%d leg={%s}", frame.SessionID, frame.LaneID, debugLeg(leg))
	if o.control == nil {
		debuglog.Printf("recv/control", "bw_probe_done_drop no_control session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}
	return o.control.OnBandwidthProbeDone(ctx, leg, frame)
}
```

Add to the switch in `WriteTo`:

```go
case protocol.TypeBandwidthProbeDone:
	return o.handleBandwidthProbeDone(ctx, leg, frame)
```

- [ ] **Step 3: Add `OnBandwidthProbeDone` to `RecvState`**

In `internal/tunnel/send/recv_state.go`, add:

```go
func (s *RecvState) OnBandwidthProbeDone(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	if s == nil || s.sender == nil {
		return nil
	}
	body, ok := frame.Body.(protocol.BandwidthProbeDoneBody)
	if !ok {
		debuglog.Printf("send/control", "invalid_body type=BW_PROBE_DONE")
		return protocol.ErrInvalidFrame
	}
	s.sender.markBandwidthProbeDone(frame.SessionID, frame.LaneID)
	debuglog.Printf("send/control", "bw_probe_done session=%d lane=%d client_bps=%d", frame.SessionID, frame.LaneID, body.ResultBps)
	return nil
}
```

- [ ] **Step 4: Build check**

```bash
go build ./internal/tunnel/send/ ./internal/tunnel/recv/
```

Expected: no errors.

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/recv/recv.go internal/tunnel/send/recv_state.go
git commit -m "recv: handle BANDWIDTH_PROBE_DONE and notify Send"
```

---

### Task 5: Tests

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Add server-ready flag tests**

Add to `internal/tunnel/send/bandwidth_probe_test.go`:

```go
func TestBandwidthProbeServerReadyNil(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}
	if !in.isBandwidthProbeServerReady(key) {
		t.Fatal("nil map should always return true")
	}
}

func TestBandwidthProbeServerReadyMarked(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 1}

	in.markBandwidthProbeDone(99, 1)

	if !in.isBandwidthProbeServerReady(key) {
		t.Fatal("marked lane should return true")
	}
	other := laneKey{sessionID: 99, laneID: 2}
	if in.isBandwidthProbeServerReady(other) {
		t.Fatal("unmarked lane should return false")
	}
}
```

- [ ] **Step 2: Run tests**

```bash
go test ./internal/tunnel/send/ -run 'TestBandwidthProbeServerReady' -v -count=1
```

Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe_test.go
git commit -m "test: add server-ready flag tests"
```

---

### Task 6: Final build and test verification

- [ ] **Step 1: Full build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 2: Full send package tests**

```bash
go test ./internal/tunnel/send/ -count=1 -timeout 120s
```

Expected: all tests pass.

- [ ] **Step 3: All project tests**

```bash
go test ./... 2>&1
```

Expected: all tests pass.

- [ ] **Step 4: Commit final state**

```bash
gofmt -w internal/...
git add -A
git diff --cached --stat
git commit -m "feat: async bandwidth probe — server waits for client per-lane"
```
