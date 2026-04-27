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
ProbeLoop
Session
Schedule Strategy
Transport
Protocol
FEC
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
- Hello only exposes `Do` and `Retry`.
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
- Send has one TUN input method: `Write`, one direct transport-bound output
  method: `WriteTo`, and one transport output channel: `Packets`. Send must not
  expose control-plane maintenance methods; the ProbeLoop adapter lives in the
  send package and uses Send's unexported control hooks.
- Do not expose semantic
  control methods such as `AcceptHello`, `AcceptHelloAck`, `ObserveLane`,
  `ReceivePing`, `ReceivePong`, or `Close` on Send.
- HELLO and HELLO_ACK state transitions must go through Session. Protocol frame
  construction stays in the caller's callback; Session must not encode frames or
  write transport packets.
- Send must not participate in HELLO_ACK admission/decision logic. The caller
  updates Send-owned lane readiness, negotiated caps, FEC profile, and probe
  state only after `Session.Ack` accepts the nonce and accepted flag.
- Transport loops call `Recv.WriteTo`. The TUN write loop consumes
  `Recv.Packets()`. Recv must not write TUN directly.
- Recv must not own transport writers. Control replies use caller-owned protocol
  frame construction and the runtime transport-bound output path.
- Do not reintroduce a tunnel loop object inside `internal/tunnel`. `Send`,
  `Recv`, and ProbeLoop are separate runtime roles and should own only
  the runtime data/dependencies they directly need.
- ProbeLoop drives probe, HELLO retry, and fallback flow. It must not become a
  raw decoded-control-frame forwarder.
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
- After code edits, run `gofmt` and at least `go build ./...`.
