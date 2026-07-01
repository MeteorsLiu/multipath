# AGENTS.md

This file provides repository guidance for Codex.

## Source Of Truth

The current codebase is being rebuilt from scratch. The design source of truth
is:

- `docs/protocol.md`
- `docs/architecture.md`

Do not infer architecture from deleted historical packages. If docs and old
mental models conflict, follow the docs.

When actively modifying protocol or architecture from a new spec document, treat
that spec as the source of truth for the change being implemented. Do not use
older implementation docs, existing code shape, or historical behavior to
preserve conflicting old protocol or architecture semantics unless the user
explicitly asks for a compatibility path.

## Critical Multipath Semantics

This project is a TUN-based multipath tunnel. It carries complete IP packets
between TUN interfaces across multiple independently scheduled lanes. It is not
an end-to-end reliable transport protocol.

FEC exists to reduce loss recovery latency for upper-layer reliable protocols
carried inside the layer-3 tunnel. Those protocols can eventually recover with
their own ARQ, but the tunnel sees that recovery only after a larger end-to-end
delay. FEC should opportunistically repair recoverable packet loss before that
upper-layer ARQ delay is paid; it must not turn the tunnel into a fully reliable
transport, add tunnel-level retransmission semantics, or chase unrecoverable loss
with reliability machinery.

QoS detection is based on FEC differential observations. Within one FEC group,
the DATA leg and REPAIR leg carry differential observations of the same source
data under the FEC rules. QoS detection may compare values derived from that
same-group relationship, for example expected source DATA bytes versus original
DATA bytes that arrived without REPAIR. FEC health is not a QoS detector input;
it drives only adaptive repair count. Do not treat primary and shadow legs as
the same capacity reference across different transport protocols. Do not
introduce cross-leg capacity heuristics such as using REPAIR-derived throughput
as primary DATA capacity or a `CapacityGap`-style signal for primary-leg QoS
decisions.

Receive-side QoS raw byte accounting is independent of FEC group lifetime.
Accepted original DATA adds `originalDataBytes` when it arrives. Received
REPAIR adds `repairBytes` when it arrives. Late DATA adds `lateDataBytes` when
the estimator has already seen that packet id as recovered. Only
`expectedBytes` is a FEC-group result; it is submitted when the group completes
or recovers and the receiver knows the source DATA byte total. A closed or
dropped FEC group must not decide whether raw DATA, REPAIR, or late-DATA bytes
can be accounted.

QoS detection must keep real sample classes separate: `originalDataBytes` are
DATA bytes received without REPAIR and without late arrivals; `expectedBytes`
are the source DATA bytes known from a completed or recovered FEC group and IP
header length parsing; `repairBytes` are received REPAIR symbol bytes;
`lateDataBytes` are original DATA bytes that arrive after the same packet id was
recovered by FEC.
Once `emitDedupe` rejects a DATA packet, it must not update
`originalDataBytes`, FEC health, recovery, emit state, or the receive FEC
window. The lane-local QoS estimator may account it once as `lateDataBytes`
only when its estimator-owned recovered-packet state has seen the packet id.
Duplicate original DATA that was already emitted as original DATA is not late
DATA. `lateDataBytes` is retained as a separate late-arrival observation only;
it must not be merged into
`originalDataBytes` or directly mark a leg limited/clear. FEC-recovered DATA
may contribute only to `expectedBytes`
through its parsed IP packet length, not to `originalDataBytes`.
Unrecovered missing DATA may
only contribute to adaptive FEC health observations such as
`DataArrived/DataExpected`; do not feed it into the QoS limited/clear detector
or convert it into synthetic DATA bytes, recovered bytes, rate samples, or
bandwidth-estimation inputs. The lane-local QoS estimator may own and update
FEC-health state only for adaptive repair-count feedback.

Derived QoS rates must preserve those names and meanings:
`expectedBps = expectedBytes / deltaT`; it is not the sum of
`originalDataBytes` and `repairBytes`. The estimator keeps only
`originalDataBytes`, `expectedBytes`, `repairBytes`, and `lateDataBytes` as
pending QoS byte classes, and a tick resets those pending counters after
converting them to rates. DATA-leg `rateGap` compares `expectedBps` with
`originalDataBps + lateDataBps`; `lateDataBytes` still remains a separate
sample class and is not merged into `originalDataBytes`. The receive-side QoS
tick is one second. The estimator stores each tick's `rateGap`, averages three
consecutive tick gaps, and compares that three-sample average with the QoS
thresholds. Do not add a separate minimum group-count gate before evaluating
QoS.

Do not add a `lossRepair` detector or use the receiver's newly requested repair
count as proof that the sender already used that count for the current receive
group. LINK_STATUS repair-count feedback affects future peer send groups after
the peer applies it; receive-side QoS must not add a separate repair-count-based
pending byte class.

QoS estimator rate state, PID correction state, limited state, and three-sample
gap window must be scoped to the DATA/REPAIR direction, for example
`data=UDP, repair=TCP` is independent from `data=TCP, repair=UDP`. The final
LINK_STATUS snapshot is aggregated per transport kind only after a direction's
role-local tick commits clear/limited state. Do not use
one transport kind's aggregated LINK_STATUS state as a gate for another
direction's DATA-leg or REPAIR-leg QoS judgment. LINK_STATUS is state-change
feedback; delivered-bps fields are auxiliary snapshot data and are not a
continuous telemetry stream. A delivered-bps-only update may emit a fresh
LINK_STATUS only when both UDP and TCP are already limited and the updated
relative bps changes the QoS-preferred primary leg.

Do not reduce this project to a single-path transport with a global UDP/TCP
fallback. Multiple lanes may be active at the same time, and the scheduler
distributes TUN packets across runnable lanes.

Use these terms consistently:

- Session: one logical tunnel between a client and a server.
- Lane: one independently scheduled logical path inside a session.
- Transport leg: the concrete carrier inside a lane, currently UDP or TCP.
- Fallback: a per-lane transport decision. If lane A's UDP leg fails, lane A
  may fall back to TCP while lane B continues using UDP.

Correct mental model:

```text
Session
├── Lane A
│   ├── UDP leg: preferred
│   └── TCP leg: fallback
└── Lane B
    ├── UDP leg: preferred
    └── TCP leg: fallback
```

Incorrect model:

```text
Session
└── one active connection
    ├── use UDP normally
    └── switch all traffic to TCP when UDP fails
```

## Module Boundaries

Public architecture boundaries:

```text
Send
Recv
Runtime RecvHandler
Session
Schedule Strategy
Transport
Protocol
FEC
Probe packages
```

Do not introduce public `Path` or `Lane` modules unless the design docs are
changed first. Do not introduce a public `Tunnel` module, a tunnel facade, or a
shared runtime-data module between Send and Recv.

Important constraints:

- Schedule Strategy exposes only `Pick`.
- Schedule Strategy does not know sessions, protocol frames, transports, or
  lane runtime objects.
- Session exposes only `Manager`, `Session`, `Hello`, and `View` with the
  interface in `docs/architecture.md`.
- Manager only owns session lifetime and creation admission:
  `Get`, `Create`, `GetOrCreate`, `GetOrDelete`, and `Delete`.
- Session only owns session id, nonce, and HELLO open/ack/retry state:
  `Open`, `Ack`, and `Do`.
- Hello only exposes `Do` and `Ack`.
- View has no exported fields and only exposes `SessionID()` and `Nonce()`.
- Session must not know lanes, transport legs, protocol frames, FEC, schedule
  strategy, caps, fallback, or packet output.
- Do not add `OpenLane`, `RunnableLanes`, `ReceiveHello`, `ReceiveHelloAck`,
  `AcceptHello`, or other lane/protocol-specific methods to Session.
- Transport works with bytes and Go network primitives. It does not know
  `Frame` or `Protocol`.
- Protocol public behavior is `Encode` and `Decode` only. `Frame` carries one
  concrete `Body`, and `Frame.Type` selects which body type is valid. Do not add
  public per-type body helper functions.
- FEC exposes shard-level `Encode` and `Reconstruct`; it does not know
  `session_id`, `lane_id`, `packet_id`, or protocol frames.
- FEC core erasure coding should use a maintained library. Local code should
  only adapt project shard/window semantics unless a different design is
  discussed first.
- UDP replies for a lane must use the same UDP socket that received the packet,
  replying to the observed remote address.
- Runtime glue must stay outside `internal/tunnel`; it only starts loops and
  waits for cancellation or errors.
- The TUN read loop belongs in `internal/tun`. Send must not own a
  `TUNReader` or application lifecycle loop.
- Send has one TUN input method: `Write`, one caller-constructed frame output
  method: `WriteFrame`, one direct transport-bound output method: `WriteTo`,
  and one transport output channel: `Packets`.
- Send owns v2 bootstrap, rebootstrap, active ping, TCP redial, optional
  bandwidth probe scheduling, and FEC capability state. Narrow exported seams
  for those runtime facts are allowed when required by transport/runtime glue:
  `Bootstrap`, `Rebootstrap`, `CloseSession`, `LaneManager`, `FECEnabled`,
  `EnableFEC`, and `OnLegFailure`.
- Do not expose semantic
  control methods such as `AcceptHello`, `AcceptHelloAck`, `ObserveLane`,
  `ReceivePing`, `ReceivePong`, or `Close` on Send.
- HELLO and HELLO_ACK state transitions must go through Session. Protocol frame
  construction stays in the caller's callback; Session must not encode frames or
  write transport packets.
- Send must not participate in HELLO_ACK admission/decision logic. `Session.Ack`
  is the nonce/accepted gate; Send-owned lane readiness is updated only by
  Send-registered callbacks after that gate accepts.
- Transport loops call `Recv.WriteTo`. The TUN write loop consumes
  `Recv.Packets()`. Recv must not write TUN directly.
- Recv must not own transport writers. Control replies use caller-owned protocol
  frame construction and the runtime transport-bound output path.
- Do not reintroduce a tunnel loop object inside `internal/tunnel`. `Send`,
  `Recv`, and runtime RecvHandler are separate runtime roles and should own only
  the runtime data/dependencies they directly need.
- v2 has no public ProbeLoop. Send starts its own HELLO retry loops, active
  ping loops, TCP dialers, and optional bandwidth-probe scheduler.
- v2 bandwidth-probe reference state belongs inside the private bwScheduler
  module. TCP reference measurements, cap-derived reference, and UDP probe
  reference/cap selection must not escape into `Send`, RecvHandler, Session,
  LaneManager, or exported methods. `Send` may pass static probe config,
  including an explicit configured reference, into the scheduler and consume
  final samples for selector quality. `Send` may retain static configured
  values, but it must not compute, override, or store dynamic/derived bw
  reference state/maps or become the bandwidth-probe state machine.
- Runtime RecvHandler is the decoded-control-frame dispatcher. It builds
  control replies through `Send.WriteFrame`, routes PONG/BW_ACK/LINK_STATUS into
  the shared LaneManager, and must not touch lane/leg internals directly.
- Runtime RecvHandler handles inbound control frames only. Local receive-side
  QoS feedback uses a separate runtime QoS writer callback injected into Recv;
  do not add outbound feedback methods to RecvHandler.
- v2 negotiates `CapLinkStatus` only with FEC. `LINK_STATUS` carries receive-side
  QoS status; it must not reintroduce global TCP fallback semantics.
- Recv packets may reuse transport read buffers; the TUN write loop must
  release each packet after writing. Transport
  `Write`/`WriteTo` implementations must finish using the provided payload
  before returning, unless the transport explicitly takes ownership by copying
  the bytes it will retain.

## Development Commands

Build:

```bash
go build ./...
```

Test:

```bash
go test ./...
```

Remote Live E2E:

Use a real remote Linux deployment for behavior that depends on real tunnel
traffic, carrier QoS, systemd service state, TCP behavior, live TUN devices,
bandwidth probing, LINK_STATUS, or UDP/TCP selector decisions. This is different
from local unit tests and from synthetic namespace tests: it validates the
program running as the deployed service against real TUN traffic and real
network shaping.

Do not write remote credentials, passwords, private host details, or temporary
access tokens into this repository or into `AGENTS.md`. Use user-provided
credentials only for the current session.

Typical workflow:

1. Push or otherwise publish the local branch that contains the change.
2. SSH to the user-provided remote Linux host with a login shell so `go`,
   service tooling, and the user's environment are loaded.
3. In the remote checkout, fetch the target branch, reset or pull to the exact
   commit being tested, and build the real binary there.

Prebuilt-binary workflow, when explicitly requested or when the remote checkout
must not be changed:

1. Confirm the remote CPU architecture with `uname -m`.
2. Cross-build locally for the remote target, for example
   `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/multipath-linux-amd64 .`.
3. Upload the binary to a remote temporary directory and mark it executable.
4. Run namespace E2E with the uploaded binary by setting
   `MULTIPATH_REAL_E2E_BIN=<remote-bin>` and
   `MULTIPATH_REAL_E2E_PREBUILT_BIN=1`.
5. If a case subset is needed for debugging, run it from a temporary copy of
   `scripts/e2e.sh`; do not edit the repository script just to select cases.

For live deployed-service validation:

1. Restart the deployed service on the remote host. The current live setup has
   used a systemd unit named `mp`; verify the unit name on the host before
   restarting it.
2. Drive traffic through the real tunnel, not through localhost shortcuts.
   For reverse-direction QoS, use reverse iperf over the TUN address, for
   example `iperf3 -c <peer-tun-ip> -R`.
3. Observe the service logs with `journalctl` while traffic and shaping are
   active. Do not rely only on a single command's exit status.

Useful remote log signals:

- `bw action=scheduler_start`
- `bw action=sample`
- `bandwidth_probe_decision`
- `runtime/qos: link_status_send`
- `runtime: link_status_apply`
- `selector action=qos_data_leg`
- `schedule_select`
- `qos_state`

For LINK_STATUS QoS validation, verify both directions explicitly: the receiving
side should emit `runtime/qos: link_status_send ... kind=1 reason=1`, the peer
should apply `runtime: link_status_apply ... kind=1 reason=1`, and DATA
selection should move to TCP with `schedule_select ... leg={tcp ... frame=type=DATA`.
For reverse tests, also confirm the iperf command is actually reverse mode and
that the limited direction matches the side expected to send LINK_STATUS.

When reporting remote live E2E results, include the tested commit, branch,
remote service state, traffic command, shaping or QoS condition, relevant log
snippets, affected lanes, and DATA leg counts where possible. If behavior differs
from local tests, treat the remote live result as the stronger signal and debug
from the live logs.

## Engineering Rules

- Verify repository-specific claims by reading files or running commands.
- If the user says a design or conclusion is wrong, re-check before defending
  it.
- Keep abstractions minimal and aligned with `docs/architecture.md`.
- Before implementing custom infrastructure, protocol helpers, encoders,
  schedulers, metrics/exporters, parsers, crypto, compression, FEC, or other
  broadly solved functionality, first research maintained existing libraries
  and use one when it fits the design. Hand-roll only after verifying no
  suitable library exists or after documenting why existing options do not fit.
- Do not add type aliases or pass-through helper APIs just to preserve old names
  or hide an existing concrete type. Use the owning type or function directly
  unless the design documents require a real semantic boundary.
- Remove type aliases and helper wrappers that do not provide ownership,
  semantic separation, or meaningful simplification.
- After code edits, run `gofmt` and at least `go build ./...`.
