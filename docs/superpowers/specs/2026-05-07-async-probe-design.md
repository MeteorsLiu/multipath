# Async Bandwidth Probe Design

**Goal:** Ensure client and server probes never run simultaneously on the
same lane. Client probes first, then notifies server via protocol frame.
Server waits for notification before starting its own probe.

## Architecture

```text
Client                                Server
  │                                     │
  ├─ ProbeLoop ─► probeBandwidth()      ├─ ProbeLoop ─► probeBandwidth()
  │   └─ runBandwidthProbeTrain()       │   └─ checks laneProbeReady
  │       └─ completeBandwidthProbeTrain│       │
  │           └─ send DONE frame ───────┼─► Recv ─► onProbeDone()
  │                                     │     └─ marks lane ready
  ◄─ no concurrent probe per lane ──────┤
```

## Protocol

### New Frame Type

```
TypeBandwidthProbeDone = <next available type>

BandwidthProbeDoneBody:
  - SessionID  uint64
  - LaneID     uint8
  - ResultBps  uint64   // client probe best step bps
```

### Frame Lifetime

1. Client `completeBandwidthProbeTrain` constructs and sends the frame on
   the lane's transport leg (same as HELLO control frames).
2. Server `Recv` decodes the frame and calls `onBandwidthProbeDone()`.
3. Server marks the lane's probe-ready flag.
4. Server's next `probeBandwidth()` tick detects the flag and starts probe.

## Send Changes

### `bandwidth_probe.go`

`completeBandwidthProbeTrain` already has `bestBps` computed. After the
metrics/log calls, add:

```go
l.sendBandwidthProbeDone(key, leg, bestBps)
```

New method:

```go
func (l *Send) sendBandwidthProbeDone(key laneKey, leg transport.LegRef, bestBps uint64) {
    // construct frame, serialize body, call transport write
}
```

The transport write for control frames follows the same pattern as HELLO.

### `probe_loop.go`

Add lane-level probe-ready state:

```go
type laneProbeState struct {
    clientDone  bool      // client-side DONE sent (for completeness)
    serverReady bool      // server-side DONE received
    clientBps   uint64    // client probe result
}
```

`ProbeLoop` gains:

```go
func (l *ProbeLoop) OnBandwidthProbeDone(sessionID uint64, laneID uint8, resultBps uint64)
```

Called from Recv when a DONE frame arrives. Sets `serverReady = true` for
the lane.

`probeBandwidth` gains a pre-check: only probe UDP if `lane.serverReady`. TCP
probe on server side also waits for DONE (since client probes TCP first).

## Recv Changes

New handler for `TypeBandwidthProbeDone` frames. Decodes body, validates
session/lane exist, calls `ProbeLoop.OnBandwidthProbeDone(sessionID, laneID, resultBps)`.

## Behavior Summary

| Side | Leg | Wait condition | Start rate | Cap | Stop |
|------|-----|----------------|------------|-----|------|
| Client | TCP | (unchanged) | 16M | 0 | plateau |
| Client | UDP | (unchanged) | 16M | 0 | loss ≥ 1% |
| Server | TCP | wait for client DONE | 16M | 0 | plateau |
| Server | UDP | wait for client DONE | 16M | 0 | loss ≥ 1% |

No cap on either side, no TCP reference dependency. Both sides probe
independently, just never on the same lane at the same time.

## Non-Goals

- No bandwidth measurement sharing between sides
- No coordinated rate caps
- No protocol-negotiated probe scheduling
- Server does not use client's result as starting rate or cap
