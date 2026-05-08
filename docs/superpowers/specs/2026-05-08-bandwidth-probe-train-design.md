# Bandwidth Probe Train Protocol Design

## Goal

Bandwidth probing must be globally serialized across the two peers:

```text
client lane N
server lane N
client lane N+1
server lane N+1
...
```

The current `BW_PROBE_DONE`-based gate is too weak because it is effectively
server-side and lane-scoped. This design replaces `BW_PROBE_DONE` with explicit
train metadata carried by `BW_PROBE` frames. A train ends when its remaining
budget reaches zero, or when the receiver times out waiting for more frames.

## Protocol Changes

`BW_PROBE` becomes the only bandwidth train control frame. `BW_PROBE_DONE` is
removed from the protocol and from runtime control flow.

The new `BandwidthProbeBody` is:

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

Field semantics:

- `TrainID` identifies one full bandwidth train.
- `ProbeID` identifies one round within that train and remains the key for ACK
  bitmaps.
- `Seq` and `Count` identify frames inside a round.
- `TrainBytesTotal` is the sender's byte budget for the train.
- `TrainBytesRemaining` is the budget left after the current frame is sent.
- `TrainBytesRemaining == 0` is the normal train-end signal.

Invalid frames:

- `Count == 0`, `Count > 64`, or `Seq >= Count`.
- `TrainBytesTotal == 0`.
- `TrainBytesRemaining > TrainBytesTotal`.
- The same `TrainID` changing `TrainBytesTotal`.

`docs/protocol.md` must be updated to remove `BW_PROBE_DONE` and document the
new `BW_PROBE` body layout.

## Train Budget

Train budget is time-based:

```text
TrainBytesTotal = effectiveCapBps * bandwidthProbeWindow / 8
```

`bandwidthProbeWindow` remains 10 seconds. The token bucket remains responsible
for instantaneous pacing. The train budget exists to define a deterministic end
point for the train and to let the peer know when the train is complete.

`effectiveCapBps` is defined as:

- configured `capBps`, when `capBps > 0`;
- `bandwidthProbeTCPRateBps`, for an uncapped TCP reference train;
- the measured TCP reference bandwidth, for the uncapped UDP train that follows
  the TCP reference.

When a bandwidth cap is configured (`capBps > 0`), probing uses UDP only. TCP
reference probing is skipped. With the current default cap of 200 Mbps, the
default behavior is UDP-only probing.

When no bandwidth cap is configured (`capBps == 0`), the lane first probes TCP
as a reference and then probes UDP.

## Serialization State Machine

The sender owns a session-level bandwidth probe gate. The gate replaces the
existing lane-scoped `bandwidthProbeServerReady` map.

The fixed order is:

```text
client lane N UDP train
server lane N UDP train
client lane N+1 UDP train
server lane N+1 UDP train
```

If `capBps == 0`, each lane includes TCP reference before UDP:

```text
client lane N TCP train
server lane N TCP train
client lane N UDP train
server lane N UDP train
client lane N+1 ...
```

The internal gate can be represented as:

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
}
```

Client starts in local phase for lane 1. Server starts in remote phase for lane
1. A local train completing moves the local side to remote phase for the same
lane and leg. Receiving the peer's train completion advances the gate to the
next phase. The local side must not advance to lane `N+1` until the remote side
has completed lane `N`.

## Receiving and Timeout

On every `BW_PROBE`, the receiver updates RX state by `TrainID`, `ProbeID`, and
round bitmap. ACK behavior remains round-based.

If `TrainBytesRemaining == 0`, the receiver:

1. Settles the remote train using the received sample data.
2. Releases the bandwidth probe gate for the next local phase.
3. Cleans up RX state for the completed train.

If the last frame is lost, the receiver releases the gate after an idle timeout:

```text
remoteTrainIdleTimeout = clamp(8 * SRTT, 500ms, 10s)
```

If the leg does not yet have an SRTT, the timeout falls back to the configured
probe timeout. Timeout settlement uses the samples already received. Timeout
only releases the bandwidth-probe gate; it must not mark the lane down.

## Error Handling

Local train failures:

- Context cancellation aborts the train.
- Leg loss or send failure stops the train, settles from existing samples, and
  releases local in-flight state.
- Bandwidth probe failures do not directly mark lanes down. Lane health remains
  owned by PING/PONG probe logic.

Receiver behavior:

- Stale train IDs are ignored.
- Future lane or future leg trains are ignored and logged.
- Duplicate or out-of-order round frames are handled by the existing
  `ProbeID`/`Seq` bitmap.
- Invalid train metadata is dropped without advancing the gate.

## Removed Runtime Pieces

Remove or stop using:

- `TypeBandwidthProbeDone`
- `BandwidthProbeDoneBody`
- `sendBandwidthProbeDone`
- `markBandwidthProbeDone`
- `EnableBandwidthProbeServerReady`
- `bandwidthProbeServerReady`
- Recv/RecvState handling for `BW_PROBE_DONE`

## Tests

Protocol tests:

- `BW_PROBE` encodes and decodes `TrainID`, `TrainBytesTotal`, and
  `TrainBytesRemaining`.
- `BW_PROBE_DONE` is no longer valid.
- Invalid remaining/total combinations are rejected.

Send tests:

- Train budget is derived from `capBps * bandwidthProbeWindow / 8`.
- `TrainBytesRemaining` decreases as frames are sent.
- The last frame carries `TrainBytesRemaining == 0`.
- With `capBps > 0`, only UDP is probed.
- With `capBps == 0`, TCP reference is probed before UDP.

Gate tests:

- Client cannot start lane `N+1` immediately after finishing client lane `N`.
- Server starts lane `N` after receiving client lane `N` completion.
- Client starts lane `N+1` only after receiving server lane `N` completion.
- Timeout releases only the bandwidth gate and does not emit lane-down events.

Verification:

```bash
go test ./internal/protocol ./internal/tunnel/send ./internal/tunnel/recv
go build ./...
```
