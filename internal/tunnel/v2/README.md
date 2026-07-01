# V2 Tunnel Runtime

This directory is the current tunnel runtime implementation. The architecture
source of truth is `docs/architecture.md`; the wire format source of truth is
`docs/protocol.md`.

The former non-v2 `internal/tunnel/send`, `internal/tunnel/recv`,
`internal/tunnel/runtime`, and `internal/tunnel/probe` packages have been
removed. New runtime work should stay in the v2 packages below instead of
reintroducing the old package layout.

## Packages

```text
internal/tunnel/v2/
├── send/          TUN ingress, lane scheduling, leg lifecycle, FEC tx
├── recv/          transport ingress, DATA/REPAIR, FEC rx, dedupe, QoS
├── runtime/       control-frame glue and LINK_STATUS writer
└── probe/
    ├── ping/      semantic ping/pong timing and RTT state
    └── bw/        semantic bandwidth-probe train, ACK, and sample logic
```

## Module Roles

Send owns the outbound data path. It accepts packets from TUN, selects a
runnable lane through the schedule strategy, writes DATA on the lane's selected
primary leg, writes optional FEC REPAIR on the shadow leg, owns TCP dialing and
redial, starts active ping loops, and starts the private bandwidth-probe
scheduler.

Recv owns the inbound data path. It decodes transport payloads, handles DATA and
REPAIR locally, keeps per-lane receive FEC windows, deduplicates emitted packet
ids per session, emits received or recovered IP packets to TUN, and forwards
control frames to `runtime.RecvHandler`.

Runtime owns decoded control-frame glue. `RecvHandler` builds HELLO_ACK, PONG,
BW_PROBE_ACK, and CLOSE replies through `Send.WriteFrame`; routes inbound PONG
and BW_PROBE_ACK into instances registered in `send.LaneManager`; routes inbound
LINK_STATUS into the lane QoS input; and owns the passive bandwidth-probe
receive side. `QoSWriter` converts Recv QoS callbacks into outbound LINK_STATUS
frames after FEC and LINK_STATUS are negotiated.

Probe packages are semantic state machines. `probe/ping` and `probe/bw` do not
import Send, Recv, Runtime, Protocol, or Transport. They emit and consume plain
semantic values through callbacks supplied by their owner.

## Data Flows

TUN packet to transport:

```text
TUN
 -> tun.Run
 -> send.Send.Write
 -> schedule.Strategy.Pick
 -> DATA on selected lane primary leg
 -> optional REPAIR on selected lane shadow leg
 -> send.Send.Packets
 -> transport.RunWriter
```

Transport DATA/REPAIR to TUN:

```text
transport.Run
 -> recv.Recv.WriteTo(observed leg)
 -> protocol.Decode
 -> per-session dedupe and per-lane rx window
 -> optional FEC recovery
 -> recv.Recv.Packets
 -> tun.RunWriter
```

Transport control frame:

```text
transport.Run
 -> recv.Recv.WriteTo(observed leg)
 -> protocol.Decode
 -> runtime.RecvHandler
 -> Session / LaneManager / QoSWriter / Send.WriteFrame
```

DATA and REPAIR never reach `recv.Handler`. RecvHandler does not handle DATA or
REPAIR and does not touch lane or leg internals directly.

## FEC And QoS

FEC is negotiated through HELLO/HELLO_ACK. Local `Send.EnableFEC()` only exposes
capability; REPAIR frames are emitted only for sessions whose negotiation
accepted FEC.

Each send lane has a lane-local FEC transmit window. DATA selected for lane X
enters lane X's window, and the resulting REPAIR keeps the same lane id. On the
receive side, each lane has a separate FEC receive window; reconstruction never
crosses lanes.

LINK_STATUS is negotiated only with FEC. Recv's QoS estimator consumes grouped
DATA/REPAIR observations from the receive FEC window. Same-leg DATA/REPAIR
samples are ignored because they have no cross-leg evidence. Cross-leg samples
estimate:

- original DATA delivery rate
- FEC-recovered expected DATA rate
- expected REPAIR rate from the group maximum packet size and decoded repair count

The estimator maintains UDP and TCP limited state for the lane. Runtime
`QoSWriter` sends LINK_STATUS snapshots carrying both leg states and their
delivered bps estimates, and Send applies each snapshot through `LaneManager`;
this is not a global UDP/TCP fallback.

## Bandwidth Probe

Bandwidth probing is driven by Send's private `bwScheduler`. The scheduler owns
target ordering, local/remote phase progress, reference selection, and final
sample consumption. Dynamic bandwidth-probe reference state does not escape into
Session, Recv, RecvHandler, or exported Send APIs.

Local probing creates a `probe/bw.BwLoop`, registers it in `LaneManager`, sends
BW_PROBE train packets, waits for BW_PROBE_ACK, and records the final sample.
Passive receive-side BW_PROBE accounting lives in Runtime's `bw.Receive`, which
returns BW_PROBE_ACK frames. When inbound BW_PROBE traffic starts or finishes,
Runtime notifies Send through opaque `LaneManager` callbacks so the scheduler can
advance without exposing its internals.

## Files

```text
internal/tunnel/v2/
├── integration_test.go
├── probe/
│   ├── bw/bw.go
│   └── ping/ping.go
├── recv/
│   ├── dedupe.go
│   ├── qos_estimator.go
│   ├── recv.go
│   ├── ref.go
│   └── rx_window.go
├── runtime/
│   ├── qos.go
│   └── recvhandler.go
└── send/
    ├── bwscheduler.go
    ├── config.go
    ├── dialer.go
    ├── lane.go
    ├── lanemanager.go
    ├── leg.go
    ├── ref.go
    └── send.go
```

## Verification

Use the repository-wide checks after edits:

```bash
go build ./...
go test ./...
```
