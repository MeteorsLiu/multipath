# Send/Session/Probe Runtime Split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the remaining runtime split by making Session the HELLO authority, extracting probe/ping and probe/bw out of Send, and turning runtime recvhandler into the single control dispatcher.

**Architecture:** Session owns only session id, nonce, and HELLO open/ack/retry state. `internal/tunnel/probe/ping` owns ping/pong timing, validation, and RTT estimation with semantic values only; it does not import `send` or `protocol`, encode protocol frames, or write transport packets. `internal/tunnel/probe/bw` owns bandwidth-probe send state, received sequence bookkeeping, ACK accounting, and bandwidth/loss sample calculation with semantic values only; it does not import `send` or `protocol`, encode protocol frames, or write transport packets. `internal/tunnel/runtime` wires Recv to Session, Send.WriteFrame, and lane-owned probe modules. Send keeps data-path scheduling, lane runtime, FEC, and transport-bound output, not the receive control dispatcher.

**Tech Stack:** Go 1.23, existing `internal/session`, `internal/tunnel/send`, `internal/tunnel/runtime`, `internal/tunnel/probe/core`, new `internal/tunnel/probe/ping`, new `internal/tunnel/probe/bw`, current protocol/session/transport packages, and the existing test suites.

**Out of Scope:** Do not delete `internal/tunnel/send/leg` in this plan. Do not introduce a public Lane module. Do not add token bucket/rate limiting to DRR. Do not redefine FEC/DRR/bitmap dedupe, which already landed separately.

---

### Task 1: Lock the Session and HELLO boundary

**Files:**
- Modify: `internal/session/session.go`
- Modify: `internal/session/session_test.go`
- Modify: `internal/tunnel/runtime/recvhandler.go`
- Modify: `internal/tunnel/runtime/recvhandler_test.go`
- Modify: `internal/tunnel/send/send.go`
- Modify: `internal/tunnel/send/session.go`
- Modify: `internal/tunnel/send/retry.go`
- Modify: `app.go`
- Modify: `runtime.go`
- Modify: `docs/architecture.md`

- [ ] **Step 1: Add tests that pin the HELLO lifecycle on Session**

Add tests that assert:

```go
func TestSessionOpenAckAndRetry(t *testing.T)
func TestRecvHandlerOnHelloRepliesWithHelloAckThroughSession(t *testing.T)
func TestRecvHandlerOnHelloAckAcceptsNonceBeforeLaneStateUpdate(t *testing.T)
```

Use the current `session.Session`, `session.Hello`, and `session.View` API only. The tests should prove that the HELLO nonce lives in `Session`, not in `Send`, and that `Ack` is the gate before any lane-state mutation.

- [ ] **Step 2: Run the focused tests and confirm the current shape is insufficient**

Run:

```bash
go test ./internal/session ./internal/tunnel/runtime ./internal/tunnel/send -run 'TestSessionOpenAckAndRetry|TestRecvHandlerOnHelloRepliesWithHelloAckThroughSession|TestRecvHandlerOnHelloAckAcceptsNonceBeforeLaneStateUpdate' -count=1
```

Expected: failures until the runtime glue stops depending on send-local HELLO handling.

- [ ] **Step 3: Move HELLO admission and reply construction into runtime glue**

Update `internal/tunnel/runtime/recvhandler.go` so it owns the HELLO control path:

```go
type RecvHandler struct {
	sessions *session.Manager
	send     *send.Send
}

func NewRecvHandler(send *send.Send, sessions *session.Manager) *RecvHandler
```

`OnHello` should use `sessions.GetOrCreate` and `sess.Do(...)` to build HELLO_ACK with `Send.WriteFrame`. `OnHelloAck` should call `sess.Ack(...)` first and only then update lane state. Keep the current send-side lane update helpers for now; do not move lane runtime into Session.

- [ ] **Step 4: Remove send-local HELLO ownership that duplicates Session**

Update `internal/tunnel/send/send.go`, `internal/tunnel/send/session.go`, and `internal/tunnel/send/retry.go` so the send-side HELLO cache is only migration plumbing, not a second source of truth. `Session.Open`, `Session.Ack`, and `Session.Hello.Retry` must remain the authoritative state for nonce and retry validity.

- [ ] **Step 5: Re-run the focused tests**

Run:

```bash
go test ./internal/session ./internal/tunnel/runtime ./internal/tunnel/send -run 'TestSessionOpenAckAndRetry|TestRecvHandlerOnHelloRepliesWithHelloAckThroughSession|TestRecvHandlerOnHelloAckAcceptsNonceBeforeLaneStateUpdate' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit the Session boundary change**

```bash
git add internal/session/session.go internal/session/session_test.go internal/tunnel/runtime/recvhandler.go internal/tunnel/runtime/recvhandler_test.go internal/tunnel/send/send.go internal/tunnel/send/session.go internal/tunnel/send/retry.go app.go runtime.go docs/architecture.md
git commit -m "refactor: move hello lifecycle into runtime glue"
```

### Task 2: Extract `probe/ping`

**Files:**
- Create: `internal/tunnel/probe/ping/ping.go`
- Create: `internal/tunnel/probe/ping/ping_test.go`
- Modify: `internal/tunnel/send/probe.go`
- Modify: `internal/tunnel/send/rtt_state.go`
- Modify: `internal/tunnel/send/event.go`
- Modify: `internal/tunnel/send/probe_loop.go`
- Modify: `internal/tunnel/send/send.go`
- Modify: `internal/tunnel/send/send_test.go`

- [ ] **Step 1: Add tests that pin ping/pong behavior in the new package**

Add tests that assert:

```go
func TestPingStartSendsMessages(t *testing.T)
func TestPingPongReturnsRTTQuality(t *testing.T)
func TestPingDropsMismatchedPong(t *testing.T)
```

Define the new package surface directly in the test target:

```go
package ping

type Message struct {
	ID     uint64
	TimeMS uint64
}

type Quality struct {
	SampleMS uint32
	SRTTMS   uint32
	RTTVarMS uint32
	Samples  uint32
}

type Ping struct {
}

func (p *Ping) Start(ctx context.Context) error
func (p *Ping) Pong(pong Message, nowMS uint64) (Quality, bool)
```

`Ping` instances bind fixed lane/transport identity, interval, timeout, and the
send callback when they are created by the owning runtime object. `Start`
actively emits `Message` values through that bound callback. `Pong` validates
`ID` and `TimeMS`, updates RTT estimation, and returns `Quality`.

The package must not import `internal/tunnel/send` and must not accept or
return `protocol.Frame` or `protocol.PingBody`.

- [ ] **Step 2: Run the focused tests and confirm the package does not exist yet**

Run:

```bash
go test ./internal/tunnel/probe/ping ./internal/tunnel/send -run 'TestPingStartSendsMessages|TestPingPongReturnsRTTQuality|TestPingDropsMismatchedPong' -count=1
```

Expected: failures until the ping package is created and wired.

- [ ] **Step 3: Move ping/pong logic out of Send**

Move the ping state-machine logic from `internal/tunnel/send/probe.go`, `internal/tunnel/send/rtt_state.go`, and `internal/tunnel/send/event.go` into `internal/tunnel/probe/ping/ping.go`. Keep lane mutation, metrics, packet encoding, and `Send.WriteFrame` outside the ping package. The ping package owns:

```text
active PING timing
PING id/time allocation
pending PING bookkeeping
PONG id/time validation
RTT estimation
```

The owning lane/transport runtime creates one `ping.Ping` for one concrete
transport path. Runtime/send adapters translate `ping.Message` to and from
protocol PING/PONG bodies and apply returned `ping.Quality` to lane quality.
The ping package itself must not import `send` or `protocol`.

- [ ] **Step 4: Route ProbeLoop through the new ping package**

Update `internal/tunnel/send/probe_loop.go` so active ping sending is owned by
the `ping.Ping` instance attached to the relevant lane/transport runtime. Add a
package-local adapter that converts ping messages to protocol frames:

```text
ping.Message -> protocol PING body -> Send.WriteFrame(ctx, PING, exact transport)
protocol PONG body -> ping.Message -> Ping.Pong -> lane RTT quality update
```

RecvHandler replies to inbound PING directly with PONG on the observed
transport. That reply path is not owned by `probe/ping`.

- [ ] **Step 5: Re-run the focused tests**

Run:

```bash
go test ./internal/tunnel/probe/ping ./internal/tunnel/send -run 'TestPingStartSendsMessages|TestPingPongReturnsRTTQuality|TestPingDropsMismatchedPong' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit the ping extraction**

```bash
git add internal/tunnel/probe/ping/ping.go internal/tunnel/probe/ping/ping_test.go internal/tunnel/send/probe.go internal/tunnel/send/rtt_state.go internal/tunnel/send/event.go internal/tunnel/send/probe_loop.go internal/tunnel/send/send.go internal/tunnel/send/send_test.go
git commit -m "refactor: extract ping runtime into probe package"
```

### Task 3: Extract `probe/bw`

**Files:**
- Create: `internal/tunnel/probe/bw/bw.go`
- Create: `internal/tunnel/probe/bw/bw_test.go`
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Modify: `internal/tunnel/send/probe.go`
- Modify: `internal/tunnel/send/probe_loop.go`
- Modify: `internal/tunnel/send/send.go`
- Modify: `internal/tunnel/send/send_test.go`

- [ ] **Step 1: Add tests that pin bandwidth-probe behavior in the new package**

Add tests that assert:

```go
func TestBWStartActiveSendsProbe(t *testing.T)
func TestBWStartPassiveSendsAck(t *testing.T)
func TestBwLoopAckUpdatesSampleAccounting(t *testing.T)
```

Define the new package surface directly in the test target:

```go
package bw

type Probe struct {
	ID        uint64
	Seq       uint16
	Count     uint16
	SendMS    uint64
	Total     uint64
	Remaining uint64
	Bytes     int
}

type Ack struct {
	ID        uint64
	Count     uint16
	Received  uint64
	FirstRXMS uint64
	LastRXMS  uint64
}

type Sample struct {
	BandwidthBps uint64
	Loss         float64
	ReferenceBps uint64
}

type BW struct {
}

func (b *BW) Start(ctx context.Context, first *Probe) (*BwLoop, error)

type BwLoop struct {
}

func (l *BwLoop) Ack(ack Ack)
```

`BW` instances bind fixed lane/transport identity, reference/cap, probe sending
callback, ACK sending callback, and sample callback when created by the owning
runtime object. `Start(ctx, nil)` actively sends probe data. `Start(ctx,
&probe)` records a received probe and sends ACKs through the bound callback.
`BwLoop.Ack` handles received ACKs for that loop.

The package must not import `internal/tunnel/send` and must not accept or
return `protocol.Frame`, `protocol.BandwidthProbeBody`, or
`protocol.BandwidthProbeAckBody`.

- [ ] **Step 2: Run the focused tests and confirm the current shape is insufficient**

Run:

```bash
go test ./internal/tunnel/probe/bw ./internal/tunnel/send -run 'TestBWStartActiveSendsProbe|TestBWStartPassiveSendsAck|TestBwLoopAckUpdatesSampleAccounting' -count=1
```

Expected: failures until the bandwidth package is created and wired.

- [ ] **Step 3: Move bandwidth-probe logic out of Send**

Move the bandwidth-probe logic from `internal/tunnel/send/bandwidth_probe.go`
into `internal/tunnel/probe/bw/bw.go`. Keep lane mutation, metrics, packet
encoding, protocol body construction, and `Send.WriteFrame` outside the
bandwidth package. The new package should own:

```text
probe validation
probe sending schedule
probe round tracking
received-bit bookkeeping
step accounting
measurement state
sample calculation
```

The owning lane/transport runtime creates one `bw.BW` for one concrete
transport path and keeps the current `*bw.BwLoop` so inbound ACKs can be routed
to `BwLoop.Ack`. Runtime/send adapters translate between `bw.Probe` / `bw.Ack`
and protocol BW_PROBE / BW_PROBE_ACK bodies. The bandwidth package itself must
not import `send` or `protocol`.

- [ ] **Step 4: Route ProbeLoop through the new bandwidth package**

Update `internal/tunnel/send/probe_loop.go` so the periodic probe decision
starts a `bw.BW` instance attached to the selected lane/transport runtime. Add
a package-local adapter that converts semantic probe values to protocol frames:

```text
bw.Probe -> protocol BW_PROBE body -> Send.WriteFrame(ctx, BW_PROBE, exact transport)
protocol BW_PROBE body -> bw.Probe -> BW.Start(ctx, &probe)
bw.Ack -> protocol BW_PROBE_ACK body -> Send.WriteFrame(ctx, BW_PROBE_ACK, observed transport)
protocol BW_PROBE_ACK body -> bw.Ack -> BwLoop.Ack
bw.Sample -> lane bandwidth quality update
```

Do not expose these adapters outside the runtime/send boundary.

- [ ] **Step 5: Re-run the focused tests**

Run:

```bash
go test ./internal/tunnel/probe/bw ./internal/tunnel/send -run 'TestBWStartActiveSendsProbe|TestBWStartPassiveSendsAck|TestBwLoopAckUpdatesSampleAccounting' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit the bandwidth extraction**

```bash
git add internal/tunnel/probe/bw/bw.go internal/tunnel/probe/bw/bw_test.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/probe.go internal/tunnel/send/probe_loop.go internal/tunnel/send/send.go internal/tunnel/send/send_test.go
git commit -m "refactor: extract bandwidth probe runtime"
```

### Task 4: Rebuild runtime recvhandler around the new modules

**Files:**
- Modify: `internal/tunnel/runtime/recvhandler.go`
- Modify: `internal/tunnel/runtime/recvhandler_test.go`
- Modify: `app.go`
- Modify: `runtime.go`
- Modify: `docs/architecture.md`

- [ ] **Step 1: Add tests that pin the new runtime constructor**

Add tests that assert the runtime glue is built from explicit modules, not from a single send-owned control adapter:

```go
func TestNewRecvHandlerWiresSessionPingAndBW(t *testing.T)
func TestRecvHandlerDispatchesControlFramesWithoutTouchingDATAorREPAIR(t *testing.T)
```

The constructor should accept the concrete runtime pieces:

```go
func NewRecvHandler(
	send *send.Send,
	sessions *session.Manager,
) *RecvHandler
```

- [ ] **Step 2: Run the focused tests and confirm the old constructor is too small**

Run:

```bash
go test ./internal/tunnel/runtime ./internal/tunnel/send -run 'TestNewRecvHandlerWiresSessionPingAndBW|TestRecvHandlerDispatchesControlFramesWithoutTouchingDATAorREPAIR' -count=1
```

Expected: failures until runtime glue stops being a thin send forwarder.

- [ ] **Step 3: Make RecvHandler the only control dispatcher**

Update `internal/tunnel/runtime/recvhandler.go` so it performs the control routing itself:

```text
HELLO -> Session + Send.WriteFrame
HELLO_ACK -> Session.Ack + lane-state update
PING -> Send.WriteFrame(ctx, PONG, observed leg) plus probe/ping state if needed
PONG -> adapter converts to ping.Message, lane-owned Ping.Pong returns quality, lane updates RTT quality
CLOSE -> Send/session cleanup path
BW_PROBE -> adapter converts to bw.Probe, lane-owned BW.Start(ctx, &probe) records and sends ACK
BW_PROBE_ACK -> adapter converts to bw.Ack, current BwLoop.Ack updates accounting and emits sample
```

`Recv` must keep DATA and REPAIR local. `RecvHandler` must not become a generic frame forwarder back into `send.control.go`.

- [ ] **Step 4: Update application wiring**

Change `app.go` and `runtime.go` so they instantiate:

```go
sessions := &session.Manager{}
handler := runtime.NewRecvHandler(in, sessions)
```

Then pass that handler into `recv.New`. Ping and bandwidth probe instances are
created and owned by the relevant lane/transport runtime, not by a global
runtime manager.

- [ ] **Step 5: Re-run the focused tests**

Run:

```bash
go test ./internal/tunnel/runtime ./internal/tunnel/send -run 'TestNewRecvHandlerWiresSessionPingAndBW|TestRecvHandlerDispatchesControlFramesWithoutTouchingDATAorREPAIR' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit the runtime glue rewrite**

```bash
git add internal/tunnel/runtime/recvhandler.go internal/tunnel/runtime/recvhandler_test.go app.go runtime.go docs/architecture.md
git commit -m "refactor: make runtime recvhandler own control dispatch"
```

### Task 5: Remove the old Send-local control wrappers and shrink the public surface

**Files:**
- Modify: `internal/tunnel/send/send.go`
- Modify: `internal/tunnel/send/control.go`
- Modify: `internal/tunnel/send/bandwidth_probe.go`
- Modify: `internal/tunnel/send/probe.go`
- Modify: `internal/tunnel/send/probe_loop.go`
- Modify: `internal/tunnel/send/send_test.go`
- Modify: `docs/architecture.md`

- [ ] **Step 1: Add tests that prove Send no longer owns the control dispatcher**

Add tests that assert:

```go
func TestSendPublicSurfaceOnlyKeepsWriteWriteFrameWriteToAndPackets(t *testing.T)
func TestProbeLoopUsesExtractedManagersInsteadOfSendControlWrappers(t *testing.T)
```

The tests should fail if new code still reaches for `AcceptHello`, `AcceptHelloAck`, `ReceivePing`, `ReceivePong`, `ReceiveBandwidthProbe`, or `ReceiveBandwidthProbeAck` as the public control surface.

- [ ] **Step 2: Run the focused tests and confirm the old wrappers are still referenced**

Run:

```bash
go test ./internal/tunnel/send ./internal/tunnel/runtime -run 'TestSendPublicSurfaceOnlyKeepsWriteWriteFrameWriteToAndPackets|TestProbeLoopUsesExtractedManagersInsteadOfSendControlWrappers' -count=1
```

Expected: failures until the old wrappers are removed or made package-local.

- [ ] **Step 3: Remove the remaining send-local control entry points**

Delete or demote the old send-local control wrappers so the documented Send surface is only:

```go
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error
func (s *Send) WriteFrame(ctx context.Context, frame protocol.Frame, to transport.LegRef) error
func (s *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error
func (s *Send) Packets() <-chan transport.Payload
```

Keep the new probe packages and runtime glue as the only control/probe entry points.

- [ ] **Step 4: Re-run the focused tests**

Run:

```bash
go test ./internal/tunnel/send ./internal/tunnel/runtime -run 'TestSendPublicSurfaceOnlyKeepsWriteWriteFrameWriteToAndPackets|TestProbeLoopUsesExtractedManagersInsteadOfSendControlWrappers' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit the Send surface shrink**

```bash
git add internal/tunnel/send/send.go internal/tunnel/send/control.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/probe.go internal/tunnel/send/probe_loop.go internal/tunnel/send/send_test.go docs/architecture.md
git commit -m "refactor: shrink send control surface"
```

### Task 6: Verify the full split and commit the plan

**Files:**
- None

- [ ] **Step 1: Run formatting and the full build**

Run:

```bash
gofmt -w internal/session/session.go internal/session/session_test.go internal/tunnel/runtime/recvhandler.go internal/tunnel/runtime/recvhandler_test.go internal/tunnel/probe/ping/ping.go internal/tunnel/probe/ping/ping_test.go internal/tunnel/probe/bw/bw.go internal/tunnel/probe/bw/bw_test.go internal/tunnel/send/send.go internal/tunnel/send/session.go internal/tunnel/send/retry.go internal/tunnel/send/probe.go internal/tunnel/send/rtt_state.go internal/tunnel/send/event.go internal/tunnel/send/bandwidth_probe.go internal/tunnel/send/probe_loop.go app.go runtime.go
go build ./...
go test ./...
```

Expected: all pass, including the send package and the new probe packages.

- [ ] **Step 2: Inspect the final diff for boundary drift**

Run:

```bash
git diff -- docs/superpowers/plans/2026-06-11-send-lane-recvhandler-rewrite-plan.md internal/session internal/tunnel/runtime internal/tunnel/probe internal/tunnel/send app.go runtime.go docs/architecture.md
```

Check that the diff does not add a public `leg` module, a public `Lane` module, or any extra transport/control abstraction beyond the approved split.

- [ ] **Step 3: Commit the plan**

```bash
git add docs/superpowers/plans/2026-06-11-send-lane-recvhandler-rewrite-plan.md
git commit -m "docs: rewrite send/session/probe split plan"
```
