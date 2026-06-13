# AGENTS.md

This file provides repository guidance for Codex.

## Source Of Truth

The current codebase is being rebuilt from scratch. The design source of truth
is:

- `docs/protocol.md`
- `docs/architecture.md`

Do not infer architecture from deleted historical packages. If docs and old
mental models conflict, follow the docs.

## Critical Multipath Semantics

This project is a TUN-based multipath tunnel. It carries complete IP packets
between TUN interfaces across multiple independently scheduled lanes. It is not
an end-to-end reliable transport protocol.

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
  `Get`, `Create`, `GetOrCreate`, and `Delete`.
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
- After code edits, run `gofmt` and at least `go build ./...`. During the v2
  migration, if `go build ./...` fails only because old `internal/tunnel/send`
  still references removed session APIs, do not change Session for old-send
  compatibility; verify with
  `go test ./internal/protocol ./internal/session ./internal/tunnel/v2/... -count=1`
  and report the old-send build gap.
