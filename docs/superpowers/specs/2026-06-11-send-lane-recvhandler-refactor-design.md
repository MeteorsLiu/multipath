# Send/Lane/RecvHandler Refactor Design

Date: 2026-06-11
Status: Draft for review
Branch: v2

## 1. Scope

This spec defines the send-side runtime refactor, the receive-handler boundary,
the per-lane FEC placement required by later QoS work, and session-scoped
receive emit dedupe.

This spec does not define QoS policy. FEC-differential QoS policy, thresholds,
feedback, and transport switching rules are defined by:

```text
docs/superpowers/specs/2026-06-11-fec-differential-leg-switching.md
```

This spec references that document only to keep the architecture compatible
with the QoS design.

## 2. Current Facts

Repository facts that this refactor must preserve or correct:

- `Session` owns session id, nonce, and HELLO open/ack/retry state.
- `Session` must not own lanes, transports, FEC, scheduler, ping, probe,
  fallback, QoS, or packet output.
- `Send` currently owns outbound lane runtime data, transport-bound output, FEC
  transmit state, ping/probe/fallback runtime state, and scheduler instances.
- Current send-side FEC transmit state is session-level.
- Current receive-side FEC state is session-level.
- Current receive emit dedupe is stored inside `rxSLCWindow` as
  `emitted map[uint32]bool`.
- Current REPAIR frames are sent through normal scheduled output and can be
  assigned to any runnable lane.
- Current receive handler naming is `ControlState` / `send.RecvState`; this
  couples receive control handling back into the send package.
- `schedule.Strategy` is already the scheduler boundary:

```go
type Lane interface {
    comparable
    Weight() uint32
}

type Strategy[L Lane] interface {
    Pick(lanes []L, cost uint32) (L, bool)
}
```

## 3. Goals

- Replace `ControlState` / `RecvState` naming with `recv.Handler` /
  `RecvHandler`.
- Keep the `recv.Handler` implementation in runtime glue, not in the send
  package.
- Keep `Send` as the owner of outbound lane runtime data.
- Keep lane runtime internal to `Send`; do not create a public Lane module.
- Add `Send.WriteFrame` so runtime receive handlers can send protocol frames
  without adding semantic control methods to `Send`.
- Move FEC transmit grouping to lane runtime.
- Move FEC receive windows to per-lane receive state.
- Move receive emit dedupe out of FEC receive windows and keep it
  session-scoped.
- Move ping and bandwidth-probe logic state into `probe/ping` and `probe/bw`.
- Use the DRR scheduler specified in
  `docs/superpowers/specs/2026-06-11-drr-scheduler-design.md`.

## 4. Non-Goals

- Do not make Lane a public runtime module.
- Do not move lanes into the Session module.
- Do not make Transport know protocol frames, FEC, scheduler, QoS, or lane
  policy.
- Do not route DATA or REPAIR through `recv.Handler`.
- Do not expose receive windows as public interfaces.
- Do not expose receive dedupe as a public interface.
- Do not implement custom bitmap storage or custom bitmap operations.
- Do not define QoS thresholds, LINK_STATUS layout, or switching policy in this
  spec.
- Do not introduce `ControlPlane`, `ControlState`, or `RecvState` in new code.

## 5. Module Boundaries

### 5.1 Session

Session owns only:

```text
session_id
nonce
HELLO open/ack/retry state
```

Session does not know:

```text
lanes
transport refs
protocol frame construction
FEC
scheduler
ping
bandwidth probe
fallback
QoS
packet output
```

### 5.2 Send

`Send` owns outbound runtime data:

```text
SessionManager reference
active session id
lane runtime data
scheduler instances
outbound packet id allocator
DATA frame construction
transport-bound output queue
send-side FEC transmit state during migration
```

`Send` does not own Session state.

`Send` exposes only these send entry points:

```go
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error
func (s *Send) WriteFrame(ctx context.Context, frame protocol.Frame, to transport.Ref) error
```

`transport.Ref` is the design name for the concrete transport reference. During
migration it may be represented by the current `transport.LegRef` type. The
zero value `transport.Ref{}` means "no specific transport was requested".

### 5.3 Lane Runtime

Lane runtime remains internal to `Send`.

Lane runtime owns:

```text
lane_id
weight
transport readiness and refs
primary/shadow transport policy
lane-local FEC transmit window
probe/ping state
probe/bw state
transport quality and fallback state
```

Lane runtime does not own scheduler fairness accounting.

### 5.4 Schedule Strategy

The schedule strategy interface remains `Pick(lanes, cost)`.

The default strategy after this refactor is DRR. DRR implementation details are
specified in:

```text
docs/superpowers/specs/2026-06-11-drr-scheduler-design.md
```

The schedule strategy does not know protocol frames, transport refs, FEC
windows, QoS state, or lane transport policy.

### 5.5 Recv

`Recv` decodes protocol frames.

`Recv` handles DATA and REPAIR locally.

`Recv` dispatches these frame types to `recv.Handler`:

```text
HELLO
HELLO_ACK
PING
PONG
CLOSE
BW_PROBE
BW_PROBE_ACK
```

### 5.6 recv.Handler

The receive handler interface is:

```go
type Handler interface {
    OnHello(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnHelloAck(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnPing(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnPong(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnClose(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnBandwidthProbe(ctx context.Context, from transport.Ref, frame protocol.Frame) error
    OnBandwidthProbeAck(ctx context.Context, from transport.Ref, frame protocol.Frame) error
}
```

Runtime glue implements this interface. The implementation may use:

```text
SessionManager
Send.WriteFrame
probe/ping logic
probe/bw logic
```

The implementation must not be named `ControlState`, `RecvState`, or
`ControlPlane`.

### 5.7 probe/ping

`probe/ping` owns active PING timing, pending PING bookkeeping, PONG
validation, and RTT estimation for one concrete lane transport path.

Package-level semantic types:

```go
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

`Ping` is created by the owning lane/transport runtime with its fixed identity,
timers, and send callback already bound. `Start` emits semantic `Message`
values through that bound callback. `Pong` validates the semantic reply and
returns RTT `Quality`.

Rules:

```text
probe/ping does not import send.
probe/ping does not import protocol.
probe/ping does not accept or return protocol frames or protocol bodies.
probe/ping does not reply to inbound PING; runtime glue replies with PONG.
probe/ping does not update lane quality directly.
```

### 5.8 probe/bw

`probe/bw` owns bandwidth probe send state, received sequence bookkeeping, ACK
accounting, and bandwidth/loss sample calculation for one concrete lane
transport path.

Package-level semantic types:

```go
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

`BW` is created by the owning lane/transport runtime with fixed identity,
reference/cap, probe send callback, ACK send callback, and sample callback
already bound.

`Start` semantics:

```text
first == nil:
  actively start local bandwidth probing and send semantic Probe values
  through the bound probe callback.

first != nil:
  receive or continue a peer bandwidth probe, record the received sequence,
  and send semantic Ack values through the bound ACK callback when required.
```

`BwLoop.Ack` handles peer ACKs for the local probe loop and emits `Sample`
through the bound callback when enough data has been collected.

Rules:

```text
probe/bw does not import send.
probe/bw does not import protocol.
probe/bw does not accept or return protocol frames or protocol bodies.
probe/bw does not update lane quality directly.
probe/bw does not choose lanes, choose transports, or implement QoS switching.
```

## 6. Send API Semantics

### 6.1 Write

`Write` is the TUN DATA entry point.

Behavior:

```text
1. Take ownership of the TUN packet.
2. Resolve the active session id.
3. Reserve the next outbound packet_id.
4. Estimate DATA frame cost.
5. Ask schedule.Strategy to pick a runnable lane.
6. If no lane is returned, drop the TUN packet.
7. Build DATA with the selected lane id.
8. Send the DATA frame through the selected lane runtime.
9. Record the DATA shard in that lane's FEC tx window when FEC is enabled.
```

`Write` is the only public method that picks a lane for TUN DATA.

### 6.2 WriteFrame

`WriteFrame` sends a caller-constructed protocol frame.

Required input:

```text
frame.SessionID must be set.
frame.LaneID must be set.
frame.Body must match frame.Type.
```

Behavior:

```text
to != transport.Ref{}:
  Send the encoded frame on exactly that transport ref.
  The transport ref must belong to frame.SessionID and frame.LaneID.

to == transport.Ref{}:
  Find the lane identified by frame.SessionID and frame.LaneID.
  Let that lane choose the transport according to frame type and lane policy.
```

`WriteFrame` does not run schedule strategy lane selection.

All current frame types may use `WriteFrame`.

If the frame's session/lane does not exist, or if a specified transport ref
does not belong to that lane, `WriteFrame` returns an error and does not emit a
packet.

## 7. Outbound Data Flows

### 7.1 DATA

```text
TUN packet
 -> Send.Write
 -> active session id
 -> reserve packet_id
 -> estimate DATA frame cost
 -> schedule.Strategy.Pick(runnable lanes, cost)
 -> selected lane runtime
 -> DATA frame with selected lane_id
 -> selected lane primary transport policy
 -> transport-bound output queue
 -> selected lane txWindow.add(packet_id, packet)
```

### 7.2 REPAIR

```text
lane txWindow reaches a full group or flush group
 -> FEC encodes repair symbol
 -> Send builds REPAIR
 -> REPAIR frame.LaneID = owning lane id
 -> owning lane shadow transport policy
 -> transport-bound output queue
```

Rules:

```text
REPAIR does not call schedule.Strategy.Pick.
REPAIR does not change FEC group ownership.
REPAIR uses the lane that owns the DATA shards.
```

### 7.3 HELLO / HELLO_ACK

```text
HELLO construction
 -> Session.Open / Hello.Do
 -> caller-owned protocol frame construction
 -> Send.WriteFrame(ctx, HELLO, exact observed/configured transport)
```

```text
HELLO_ACK construction
 -> Session.Ack result is handled by runtime glue
 -> caller-owned protocol frame construction
 -> Send.WriteFrame(ctx, HELLO_ACK, observed transport)
```

HELLO and HELLO_ACK state transitions go through Session. Session does not
encode frames and does not write transport packets.

### 7.4 PING / PONG

```text
Outbound PING:
  lane-owned probe/ping Ping.Start emits ping.Message.
  Runtime/send adapter converts ping.Message to protocol PING.
  Send.WriteFrame(ctx, PING, exact transport)
```

```text
Inbound PING:
  Recv calls recv.Handler.OnPing(ctx, observed transport, frame).
  Runtime handler converts the protocol body to a semantic ping.Message.
  Runtime handler builds PONG with the same ID and TimeMS.
  Send.WriteFrame(ctx, PONG, observed transport)
```

```text
Inbound PONG:
  Recv calls recv.Handler.OnPong(ctx, observed transport, frame).
  Runtime handler converts the protocol body to ping.Message.
  Runtime handler routes the message to the lane-owned Ping.Pong.
  Ping.Pong returns ping.Quality when the PONG matches.
  lane runtime updates transport RTT quality from ping.Quality.
```

### 7.5 BW_PROBE / BW_PROBE_ACK

```text
Outbound BW_PROBE:
  lane-owned probe/bw BW.Start(ctx, nil) starts active probing.
  BW emits semantic bw.Probe values through its bound probe callback.
  Runtime/send adapter converts bw.Probe to protocol BW_PROBE.
  Send.WriteFrame(ctx, BW_PROBE, exact transport)
```

```text
Inbound BW_PROBE:
  Recv calls recv.Handler.OnBandwidthProbe(ctx, observed transport, frame).
  Runtime handler converts the protocol body to bw.Probe.
  Runtime handler routes the probe to lane-owned BW.Start(ctx, &probe).
  BW records the received sequence.
  BW emits semantic bw.Ack through its bound ACK callback when required.
  Runtime/send adapter converts bw.Ack to protocol BW_PROBE_ACK.
  Send.WriteFrame(ctx, BW_PROBE_ACK, observed transport)
```

```text
Inbound BW_PROBE_ACK:
  Recv calls recv.Handler.OnBandwidthProbeAck(ctx, observed transport, frame).
  Runtime handler converts the protocol body to bw.Ack.
  Runtime handler routes the ACK to the current lane-owned BwLoop.Ack.
  BwLoop.Ack updates ACK accounting and emits bw.Sample when complete.
  lane runtime updates transport bandwidth quality from bw.Sample.
```

`probe/ping` and `probe/bw` own logic state only. Runtime/send adapters do all
protocol conversion and all `Send.WriteFrame` calls. The probe packages do not
encode frames, write transport packets, choose lanes, choose transports, or own
lane lifecycle.

## 8. Inbound Data Flows

### 8.1 Receive State Shape

`Recv` keeps receive state per session.

Inside each session receive state:

```text
emit dedupe: session-scoped
rxWindow: per lane
```

The receive windows are implementation details:

```text
not exported
no public interface
not read or mutated by recv.Handler
```

### 8.2 Emit Dedupe

Receive emit dedupe is separate from rxWindow.

Properties:

```text
session-scoped
internal to recv
used for original DATA and recovered DATA
keyed by packet_id
```

The dedupe bitmap backend must be:

```text
github.com/bits-and-blooms/bitset
```

Local code may maintain the packet id window base and eviction policy. Local
code must not implement custom bitmap storage or custom bitmap operations.

### 8.3 DATA

```text
Recv DATA lane=X packet_id=N
 -> find session receive state
 -> session emit dedupe Mark(N)
 -> if duplicate:
      do not write TUN
      do not add DATA to rxWindow
      do not count DATA arrival for FEC-differential QoS
 -> if first:
      rxWindow[X].addData(N, payload)
      rxWindow[X] records DATA arrival information needed by QoS
      emit IP packet to TUN
      if rxWindow[X] finds a recoverable group:
          recover the missing packet
          pass recovered packet through session emit dedupe
```

### 8.4 REPAIR

```text
Recv REPAIR lane=X
 -> find session receive state
 -> rxWindow[X].addRepair(base_packet_id, source_span, symbol)
 -> rxWindow[X] records REPAIR arrival information needed by QoS
 -> if exactly one DATA symbol is recoverable:
      recover packet_id=N
      session emit dedupe Mark(N)
      if first:
          emit recovered packet to TUN
      if duplicate:
          drop recovered output
```

REPAIR has no TUN output identity by itself and does not use emit dedupe before
entering rxWindow.

## 9. FEC Placement

### 9.1 Transmit

Each lane runtime owns its own txWindow.

DATA selected for lane X is inserted into lane X's txWindow only after the DATA
frame is assigned to lane X.

FEC group ownership is lane-local:

```text
DATA selected for lane X
 -> lane X txWindow
 -> lane X REPAIR
 -> REPAIR frame.LaneID = X
```

### 9.2 Receive

Each session receive state owns per-lane rxWindows.

DATA and REPAIR use `frame.LaneID` to select the rxWindow.

FEC reconstruction never crosses rxWindow boundaries.

## 10. QoS Compatibility Notes

FEC-differential QoS is specified by:

```text
docs/superpowers/specs/2026-06-11-fec-differential-leg-switching.md
```

This refactor provides the prerequisites that spec needs:

```text
per-lane FEC txWindow
per-lane FEC rxWindow
REPAIR sent by owning lane without re-scheduling
lane-local primary/shadow transport policy
receive-side DATA/REPAIR arrival bookkeeping next to rxWindow
```

The QoS spec uses the term `leg` for UDP/TCP carrier roles. This refactor keeps
that concept inside lane runtime as primary/shadow transport policy. It does
not introduce a public leg module.

This spec does not define:

```text
QoS thresholds
LINK_STATUS wire layout
LINK_STATUS refresh/TTL behavior
primary/shadow switching rules
QoS metrics names
```

## 11. Migration Steps

1. Add `recv.Handler` and move receive handler implementation out of the send
   package into runtime glue.
2. Add `Send.WriteFrame(ctx, frame, to)`.
3. Move receive emit dedupe out of `rxSLCWindow` into session receive state.
4. Replace receive dedupe storage with `github.com/bits-and-blooms/bitset`.
5. Change receive FEC state from one session rxWindow to per-lane rxWindows.
6. Move send FEC txWindow from session send state to lane runtime.
7. Change REPAIR send flow so REPAIR uses the owning lane and does not call
   `schedule.Strategy.Pick`.
8. Add DRR strategy from the DRR spec and make it the default scheduler.
9. Split ping and bandwidth-probe logic state into `probe/ping` and `probe/bw`
   without giving those packages transport-writing responsibility.

## 12. Testing Requirements

| Area | Required check |
|---|---|
| recv.Handler | Non-DATA/REPAIR frames dispatch to `recv.Handler` |
| naming | New code does not introduce `ControlState`, `RecvState`, or `ControlPlane` |
| Write | `Write` is the only public DATA lane-picking entry point |
| WriteFrame | Explicit transport ref writes to exactly that transport |
| WriteFrame | Zero transport ref delegates only to the specified lane policy |
| DATA receive | Duplicate DATA is dropped before rxWindow and before TUN output |
| recovered DATA | Recovered packet uses the same session emit dedupe |
| bitmap | Emit dedupe uses `github.com/bits-and-blooms/bitset` |
| FEC TX | DATA on lane X creates REPAIR with `LaneID = X` |
| FEC TX | REPAIR does not call scheduler Pick |
| FEC RX | DATA/REPAIR for lane X use rxWindow X |
| probe/ping | Module owns logic state but not protocol encoding or transport write |
| probe/bw | Module owns logic state but not protocol encoding or transport write |
| QoS boundary | This spec only references the FEC differential QoS spec |
