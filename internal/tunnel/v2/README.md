# V2 Send / Probe / Runtime / Recv Refactor

Spec: `docs/superpowers/specs/2026-06-11-send-lane-recvhandler-refactor-design.md`
Plan: `docs/superpowers/plans/2026-06-11-send-lane-recvhandler-rewrite-plan.md`

A clean-room implementation of the send/receive refactor in `internal/tunnel/v2/`,
built strictly to the spec module boundaries rather than ported from the old
`internal/tunnel/{send,recv}`.

`transport.Ref` is the spec 5.2 design name for a concrete transport reference;
v2 packages alias it locally (`type Ref = transport.LegRef`) so the global
`internal/transport` package is untouched. There is no public `leg` module — the
leg concept lives inside lane runtime as primary/shadow transport policy
(spec 10).

## Module boundaries

```
session (existing)   HELLO authority: session_id, nonce, open/ack/retry
        ▲
        │
runtime/RecvHandler   the glue (spec 5.6). Owns per-path probe/ping and
        │             probe/bw instances. Converts protocol <-> semantic and
        │             drives all output through Send.WriteFrame.
        ├──────────────► probe/ping   pure ping/pong timing + RTT (no send/protocol)
        ├──────────────► probe/bw     pure bandwidth probe logic (no send/protocol)
        ├──────────────► send         thin: encode, schedule, lane send, FEC tx
        └──────────────► recv         decode, per-lane FEC rx, dedupe, dispatch
```

### send (thin)

Per spec 5.2, Send owns only the outbound data path and exposes exactly:

```go
func (s *Send) Write(ctx, *packetbuf.Packet) error   // TUN DATA, runs scheduler
func (s *Send) WriteFrame(ctx, protocol.Frame, Ref)  // any frame, no scheduler
func (s *Send) WriteTo(ctx, Ref, *packetbuf.Packet)
func (s *Send) Packets() <-chan transport.Payload
```

Send does **not** import probe/ping or probe/bw and holds no ping/bw state.
Its internals: lane runtime (unexported), DRR scheduler, packet-id allocator,
DATA construction, per-lane FEC transmit window, transport output queue.

**Lane primary/shadow roles.** Each lane carries a `primaryKind` (the carrier
kind currently acting as primary; initial = UDP). `primaryTransport()` returns
the DATA leg, `shadowTransport()` the REPAIR leg (the other kind). When only one
leg is ready both return the *same* leg — DATA and REPAIR share one link, the
correct single-transport degradation, which the receiver detects on its own (no
notification). `setPrimary(kind)` is a reserved reversal entry point: it flips
the role so REPAIR routing reverses with it, but no QoS switching policy,
LINK_STATUS, or TTL machinery exists this round.

### probe/ping, probe/bw

Pure logic packages. They import neither `send` nor `protocol`, deal only in
semantic values (`ping.Message`/`ping.Quality`, `bw.Probe`/`bw.Ack`/`bw.Sample`),
and never encode frames or touch transports. Instances are created with their
send callbacks already bound.

Verified:

```
probe/ping multipath imports: NONE
probe/bw   multipath imports: NONE
send imports v2/probe:        NONE
```

### runtime/RecvHandler (the glue)

Implements `recv.Handler`. This is the only component that knows both protocol
frames and the semantic probe packages. It owns a `map[probeKey]*ping.Ping` and
`map[probeKey]*bw.BW` keyed by `{session, lane, transport}`, plus the active
`map[trainID]*bw.BwLoop`. Every probe callback is an adapter that builds a
protocol frame and calls `Send.WriteFrame`.

### recv (per-lane receive)

`Recv` decodes transport-bound frames, handles DATA and REPAIR locally, and
dispatches the seven control frame types to a `recv.Handler` (which
`runtime.RecvHandler` structurally satisfies — verified by
`var _ recv.Handler = (*RecvHandler)(nil)`). DATA and REPAIR never reach a
Handler.

Per session it keeps **per-lane** `rxSLCWindow`s (FEC reconstruction never
crosses lanes, spec 9.2) and one **session-scoped** emit dedupe over
`bits-and-blooms/bitset` (spec 8.2); a duplicate DATA is dropped before the
window and before TUN. The FEC window and dedupe algorithms are reused from the
old recv, unexported.

Beside each lane's window sits a reserved **per-lane QoS arrival ledger**
(`laneArrivalStats`, spec 8.3/8.4): it counts wire arrivals by `{carrier kind ×
DATA|REPAIR}`. Its intended consumer compares the kind carrying DATA against the
kind carrying REPAIR — equal means single-transport, so differential detection
is skipped. This round only writes the ledger; nothing reads it for a decision,
and it is not exported. recv imports neither send nor the probe packages.

## Data flows (spec 7)

| Flow | Path |
|------|------|
| DATA (7.1) | `Send.Write` → reserve id → DRR pick → DATA on lane primary transport → lane FEC txWindow |
| REPAIR (7.2) | lane txWindow full/flush → FEC encode → REPAIR on owning lane shadow transport (no scheduler) |
| HELLO (7.3) | `Session.Open` → frame → `Send.WriteFrame`; inbound → `RecvHandler.OnHello` → `sessions.GetOrCreate` → HELLO_ACK via `Send.WriteFrame` |
| HELLO_ACK (7.3) | inbound → `RecvHandler.OnHelloAck` → `sess.Ack` validates nonce |
| out PING (7.4) | `RecvHandler.StartPing` → `ping.Ping.Start` → adapter → `Send.WriteFrame(PING)` |
| in PING (7.4) | `RecvHandler.OnPing` → builds PONG → `Send.WriteFrame(PONG)` |
| in PONG (7.4) | `RecvHandler.OnPong` → `ping.Message` → lane-path `Ping.Pong` → `ping.Quality` |
| out BW (7.5) | `RecvHandler.StartBandwidthProbe` → `bw.BW.Start(nil)` → adapter → `Send.WriteFrame(BW_PROBE)`; returns tracked `BwLoop` |
| in BW (7.5) | `RecvHandler.OnBandwidthProbe` → `bw.Probe` → lane-path `BW.Start(&probe)` → ACK via bound callback → `Send.WriteFrame(BW_PROBE_ACK)` |
| in BW_ACK (7.5) | `RecvHandler.OnBandwidthProbeAck` → `bw.Ack` → tracked `BwLoop.Ack` → `bw.Sample` |
| in DATA (8.3) | `Recv.WriteTo` → session dedupe `mark` (dup→drop) → first: account + per-lane `rxWindow.addData` → emit to TUN → recover if a group completes |
| in REPAIR (8.4) | `Recv.WriteTo` → account + per-lane `rxWindow.addRepair` → if exactly one DATA recoverable: reconstruct → session dedupe → emit (or drop late dup) |

## FEC (spec 9)

Per-lane transmit window (4+1 SLC, hardcoded). DATA selected for lane X enters
lane X's `txSLCWindow` only; a full or flushed group produces a REPAIR with
`LaneID == X`, sent on the lane's shadow transport. REPAIR never calls the
scheduler. On receive, each lane has its own `rxSLCWindow`; reconstruction never
crosses lanes, and recovered DATA passes the session-scoped emit dedupe so a late
original is not double-emitted.

## Files

```
internal/tunnel/v2/
├── README.md
├── integration_test.go         round-trip data-flow tests (7.3/7.4/7.5)
├── probe/
│   ├── ping/ping.go            ping/pong timing + RTT (RFC 6298)
│   └── bw/bw.go                bandwidth probe send/ack/sample
├── runtime/
│   └── recvhandler.go          recv.Handler: owns probes, adapters, dispatch
├── send/
│   ├── ref.go                  type Ref = transport.LegRef
│   ├── config.go               SessionManager / EnableFEC / BootstrapLanes
│   ├── lane.go                 unexported laneRuntime, primary/shadow, FEC txWindow
│   ├── send.go                 Write / WriteFrame / WriteTo / Packets
│   └── e2e_test.go             Send→Recv FEC recovery + dedupe
└── recv/
    ├── ref.go                  type Ref = transport.LegRef
    ├── recv.go                 Recv, Handler, per-session state, dispatch
    ├── rx_window.go            per-lane FEC receive window (unexported)
    ├── dedupe.go               session-scoped bitset emit dedupe (unexported)
    └── accounting.go           reserved per-lane QoS arrival ledger
```

## Tests

```
go test ./internal/tunnel/v2/... -race
ok  .../v2            (HELLO via Session, control routing, PING/PONG and
                       BW round-trips, Send→Recv FEC recovery)
ok  .../v2/probe/bw
ok  .../v2/probe/ping
ok  .../v2/recv       (per-lane window isolation, dup drop + no-account,
                       accounting by kind×category, control dispatch)
ok  .../v2/runtime
ok  .../v2/send       (primary/shadow roles, single-leg degrade, REPAIR on shadow)
```

## Out of scope (spec §4)

No public Lane module; lanes are not moved into Session; no QoS thresholds or
LINK_STATUS; DRR has no token bucket; recv-side rxWindow/dedupe changes are a
separate migration step.
