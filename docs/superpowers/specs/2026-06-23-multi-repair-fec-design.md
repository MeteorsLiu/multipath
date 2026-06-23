# Adaptive Multi-Repair Packet FEC Design

## Goal

Build an end-to-end adaptive FEC design for the v2 tunnel that can survive
severe DATA loss without turning FEC health into a QoS decision signal.

The current `4+1` FEC design can recover only one missing DATA packet per FEC
group. Under heavy loss, too many groups become unrecoverable, so the receiver
cannot know the true source bytes of missing IP packets. That breaks the QoS
estimator loop because expected DATA bytes cannot be computed accurately.

This design extends packet-level FEC from `4+1` to adaptive `4+N`, where `N` is
a lane-local repair count in the range `1..4`. The receiver uses FEC loss
pressure to request more REPAIR packets. When groups become recoverable again,
the receiver derives accurate source bytes from the recovered IP packets'
headers and feeds those bytes into the QoS estimator.

## Core Principles

- FEC remains opportunistic packet loss repair for a layer-3 tunnel.
- FEC must not add tunnel-level retransmission or reliable transport semantics.
- FEC health does not directly commit QoS limited or clear state.
- QoS rate estimation is based on DATA source bytes versus DATA-leg arrival,
  not on REPAIR arrival ratio alone.
- Multi-repair FEC exists to make missing DATA packets recoverable often enough
  that their real IP lengths can be known.
- Send-side FEC control is lane-local. It is not keyed by transport kind,
  target role, or DATA/REPAIR direction.
- REPAIR still follows the lane shadow role. The leg selector chooses the
  concrete UDP/TCP leg.
- Session and Transport boundaries do not change.

## Packet-Level FEC Model

The design follows the packet-level model used by projects such as kcp-go with
`github.com/klauspost/reedsolomon`:

- one DATA shard is one complete IP packet;
- one FEC group contains `source_span` DATA shards;
- `source_span` stays `1..4` initially;
- a full group is four DATA packets;
- all shards in one group are equal length only inside the FEC calculation;
- short DATA packets are virtually zero-padded for coding;
- DATA packets are not padded on the DATA leg;
- recovered DATA shards are truncated by parsing the recovered IP header total
  length.

For one group:

```text
DATA sizes = 1000, 20, 80, 60
repair_symbol_size = 1000
```

Every REPAIR symbol for this group has `repair_symbol_size` bytes:

```text
repair_count = 1 -> 1 * 1000 bytes
repair_count = 4 -> 4 * 1000 bytes
```

The bandwidth cost is accepted for this design. Avoiding it would require
source-symbol fragmentation or size-bucketed grouping, which is outside this
change.

## Protocol

### REPAIR

The REPAIR frame body does not change:

```text
base_packet_id uint32
key            uint16
source_span    uint8
repair_symbol  bytes[repair_symbol_size]
```

Multiple REPAIR frames for the same group reuse the same `base_packet_id` and
`source_span` and carry different `key` values.

`key` remains a coding-coefficient identifier. It is not a packet id, repair
target, lane id, transport kind, or role. The sender allocates a unique key for
each REPAIR equation. The receiver uses the keys for the received REPAIR
symbols to reconstruct a compact Reed-Solomon codec for that group.

There is no REPAIR count field in the REPAIR frame. The number of REPAIR frames
is the count:

```text
repairCount = 3

REPAIR base_packet_id=100 source_span=4 key=20 repair_symbol=...
REPAIR base_packet_id=100 source_span=4 key=21 repair_symbol=...
REPAIR base_packet_id=100 source_span=4 key=22 repair_symbol=...
```

### LINK_STATUS

`LINK_STATUS.status` is reused as the unified state byte. There is no separate
FEC control frame.

```text
status = UUUU TTTT

high uint4 = UDP state
low uint4  = TCP state

each uint4:
  bit0    = QoS limited state
  bits1-3 = repairCount - 1
```

The default repair count is `1`, which is the current `4+1` behavior. Therefore
repair bits `000` mean one repair packet, not zero repair packets.

Initial valid per-transport state values are:

```text
0000 = repair count 1, QoS clear
0001 = repair count 1, QoS limited
0010 = repair count 2, QoS clear
0011 = repair count 2, QoS limited
0100 = repair count 3, QoS clear
0101 = repair count 3, QoS limited
0110 = repair count 4, QoS clear
0111 = repair count 4, QoS limited
```

Values `1000..1111` are outside the current maximum repair count and are not
emitted by this design.

Encoding:

```text
repairCode = repairCount - 1
state      = (repairCode << 1) | qosLimitedBit
status     = (udpState << 4) | tcpState
```

Decoding:

```text
qosLimited  = (state & 0b0001) != 0
repairCount = ((state >> 1) & 0b0111) + 1
```

QoS limited state remains per transport kind. Repair count bits are the adaptive
FEC control signal carried by the same uint4 state value.

Because repair count is lane-local rather than transport-kind-local, outbound
LINK_STATUS writes the same repair-count bits into both the UDP and TCP uint4
states. The QoS bit in each uint4 remains transport-kind specific.

Example:

```text
UDP state with repairCount = 3 and QoS limited = true

UDP nibble = 0101
```

Inbound LINK_STATUS applies the QoS bits to the selector as today and applies
the decoded repair count to the matching lane's FEC transmit state. The sender
does not interpret the repair count as a target transport or target role; it is
the number of REPAIR frames to emit for future FEC groups on that lane.

The implementation uses the existing lane QoS input path. It does not add an
external FEC setter on Send or LaneManager:

```text
RecvHandler.OnQoS(LINK_STATUS)
 -> decode UDP/TCP QoS bits
 -> decode repairCount from the status nibbles
 -> LaneManager.LookupQoS(session_id, lane_id)
 -> QoSInput.OnQoSStatus(udpLimited, udpBps, tcpLimited, tcpBps, repairCount)
 -> laneQoSInput.OnQoSStatus(...)
 -> lane.leg.observeQoSStatus(...)
 -> lane.setFEC(repairCount)
```

`lane.setFEC` is an unexported lane-internal method. It stores only the current
lane repair count used by future FEC groups.

LINK_STATUS is emitted when either committed QoS state changes or the requested
lane repair count changes. Delivered-bps fields remain auxiliary snapshot data;
they are not a continuous telemetry stream.

The sender does not need target role, DATA/REPAIR direction, or
transport-kind-scoped FEC state from LINK_STATUS. The selector continues to
choose primary and shadow roles.

LINK_STATUS is a peer-to-local-send feedback frame. When local runtime receives
it, the frame updates the local send lane that matches `(session_id, lane_id)`.
It does not update the local receive FEC window and does not participate in
receive-side estimation.

## Codec

The FEC package remains shard-level. It does not know sessions, lanes, packet
ids, protocol frames, transport legs, QoS, or adaptive policy.

Public method names stay the same, but the key argument becomes a slice:

```go
func (c *Codec) Encode(shards [][]byte, keys []uint16) error
func (c *Codec) Reconstruct(shards [][]byte, keys []uint16) error
```

`len(shards)` must equal `dataShards + repairShards`. `len(keys)` must equal
`repairShards`.

Codec behavior:

- DATA shards may have different lengths.
- Coded shards are based on the maximum known shard length in the group.
- Each key identifies one repair equation generated from the existing TinyMT
  coefficient generator.
- Encode and reconstruct use `github.com/klauspost/reedsolomon` with a custom
  matrix.
- Recovered DATA shards are returned at coded shard length.
- Recv owns IP total length parsing and truncation.

Existing `4+1` behavior is the `repairShards=1` case with a single key in the
slice.

## Sender Design

### Ownership

Send owns:

- lane-local FEC transmit windows;
- lane-local adaptive repair count;
- REPAIR key allocation;
- REPAIR frame emission;
- primary/shadow role selection through the existing leg selector.

Send does not own QoS estimation, FEC health analysis, or receive-side source
byte accounting.

### Lane State

Each lane has:

```text
fecRepairCount: 1..4
default: 1
```

The repair count is lane-local. It is not stored as UDP state or TCP state.
The only write path for peer-requested repair count is the lane-internal
`setFEC(repairCount)` call reached through `laneQoSInput.OnQoSStatus`.

### Applying Peer Feedback

Inbound LINK_STATUS changes the local send lane's future behavior:

```text
peer LINK_STATUS
 -> local runtime control dispatcher
 -> LaneManager QoS input for (session_id, lane_id)
 -> laneQoSInput.OnQoSStatus(..., repairCount)
 -> QoS bits update lane selector quality
 -> lane.setFEC(repairCount)
```

The send lane does not inspect why the peer requested that count. It only
stores the current lane repair count and uses it when emitting future FEC
groups.

### Emission Flow

When `Send.Write` selects a lane and writes a DATA frame successfully:

```text
TUN packet
 -> scheduler picks lane
 -> DATA written on lane primary role
 -> DATA packet copied into that lane's FEC transmit window
```

When the transmit window emits a group, the group snapshots the lane's current
`fecRepairCount`. The group uses the maximum DATA packet length as
`repair_symbol_size`, creates that many independent REPAIR equations, and emits
one REPAIR frame for each equation. Each REPAIR frame carries the group's
`base_packet_id`, the group's `source_span`, and that repair equation's `key`.

All emitted REPAIR frames for the group go through the lane shadow role. The
selector chooses the concrete transport leg for that role.

Partial groups created by the existing flush path use the same repair count.
Their `source_span` is the number of protected DATA packets in that partial
group.

### Repair Count Change Semantics

A repair count change affects future FEC emission only:

- already-emitted groups are not revisited;
- no immediate REPAIR burst is sent when the value changes;
- the transmit window is not cleared or rebuilt;
- a pending group uses the repair count that is current when the group emits
  REPAIR;
- every emitted group sends exactly `fecRepairCount` REPAIR frames.

For one group and `fecRepairCount = N`:

```text
DATA group base_packet_id=100 source_span=4
 -> REPAIR #1 base_packet_id=100 source_span=4 key=K0
 -> REPAIR #2 base_packet_id=100 source_span=4 key=K1
 ...
 -> REPAIR #N base_packet_id=100 source_span=4 key=K(N-1)
```

## Receiver Design

### Ownership

Recv owns:

- lane-local FEC receive windows;
- DATA and REPAIR storage;
- FEC recovery;
- group outcome accounting;
- QoS estimator input generation;
- adaptive repair-count recommendation.

Recv does not write transport packets directly. It reports local QoS/FEC status
through the existing QoS writer callback path.

### Receive Window

The receive window is a lane-local FEC-only internal module. It does not know
QoS, adaptive repair policy, LINK_STATUS, or how callers will consume group
results. It only stores FEC shards, determines group state, builds
reconstruction inputs, and releases buffers it owns.

The receive window has two core stores:

```text
recent DATA cache:
  packet_id -> DATA shard

groups:
  (base_packet_id, source_span) -> group
```

DATA does not create a group, because a DATA frame does not carry
`source_span`. A REPAIR frame declares the group by carrying
`base_packet_id` and `source_span`; only then can the receiver know the exact
group boundary, including partial groups created by flush.

The group data model is direct:

```go
type rxGroupKey struct {
    basePacketID uint32
    sourceSpan   uint8
}

type rxGroup struct {
    key     rxGroupKey
    data    []*rxDataShard  // len == sourceSpan
    repairs []rxRepairShard // unique repair keys for this group
}
```

`group.data[i]` corresponds to:

```text
packet_id = base_packet_id + i
```

The window keeps `recentData` as the owner of DATA buffers. Groups reference
those DATA shards; they do not copy or release DATA. Groups own REPAIR buffers
and release them when the group completes, recovers, expires, or the window is
closed.

All receive-window functions and helper types stay unexported because this is
an internal Go module. The window exposes only the minimum facts other receive
side code needs:

```go
type rxWindowResult struct {
    recoverable []rxRecoverable
    done        []rxGroupDone
}

type rxRecoverable struct {
    group       rxGroupKey
    missingMask uint8
}

type rxGroupDone struct {
    group        rxGroupKey
    dataArrived  uint8
    dataExpected uint8
    recovered    bool
    expired      bool
}
```

The result intentionally does not expose QoS samples, estimator inputs,
adaptive-policy decisions, DATA/REPAIR direction state, or the group's internal
slices. Callers that need those policies derive them outside the FEC window from
the FEC facts and from the transport/frame context they already have.

The current "one repair per base packet id" storage is replaced by "many repair
shards per group".

### Recovery Flow

DATA enters the FEC receive window only after duplicate DATA has already been
rejected by Recv. The window stores the DATA shard in `recentData`, then attaches
it to any already-open group whose range contains that `packet_id`.

REPAIR is stored under its group `(base_packet_id, source_span)`. If the group
does not exist, the window creates it, allocates `group.data` with
`len == source_span`, and attaches any matching DATA already present in
`recentData`. A duplicate REPAIR key inside the same group is dropped and is not
an independent equation.

Recovery is attempted when:

```text
known DATA shards + known REPAIR shards >= source_span
and at least one DATA shard is missing
```

In code terms:

```text
knownData  = count(group.data[i] != nil)
missing    = source_span - knownData
recoverable = missing > 0 && knownData + len(group.repairs) >= source_span
complete    = missing == 0
```

The window builds a compact shard set from all received DATA shards and the
received REPAIR shards selected for reconstruction. The caller creates a codec
with `repairShards = len(selectedRepairKeys)` and calls
`Reconstruct(shards, keys)`.

After successful recovery, Recv parses recovered DATA shards as IP packets,
uses the IP total length to truncate each packet, emits recovered DATA to TUN
only if that packet has not already been emitted, and tells the receive window
to close the recovered group.

If a group expires before recovery, the receive window closes it and returns
`rxGroupDone{expired: true, dataArrived, dataExpected}`. The window does not
synthesize missing source bytes and does not decide how that expired-group fact
affects QoS or adaptive FEC.

## Source Byte Accounting

The estimator must not use:

```text
source_span * repair_symbol_size
```

as source bytes. That shortcut overestimates mixed-size packet groups.

For a recovered or complete group:

```text
actual_data_bytes      = sum(IP packet lengths that arrived on DATA leg)
recovered_source_bytes = sum(IP packet lengths recovered by FEC)
expected_source_bytes  = actual_data_bytes + recovered_source_bytes
repair_wire_bytes      = sum(REPAIR payload lengths received)
```

`expected_source_bytes` is the receiver's best known source-byte total for that
group. It is known only when all DATA packets in the group are present or
recovered.

`actual_data_bytes` is DATA-leg delivery only. Recovered DATA does not increase
DATA-leg actual delivery.

`repair_wire_bytes` is used for REPAIR leg rate/cost accounting. It does not
directly imply source bytes.

For unrecoverable groups:

- known DATA packet count is valid;
- missing DATA packet count is valid from `source_span`;
- missing DATA bytes are unknown;
- no expected source bytes are emitted for QoS rate estimation.

## QoS Estimator Integration

The estimator continues to make QoS limited/clear decisions from rate evidence
scoped to DATA/REPAIR direction and role.

Multi-repair FEC changes the inputs:

1. DATA arrivals still contribute DATA-leg actual bytes.
2. REPAIR arrivals contribute REPAIR-leg wire bytes.
3. Completed or recovered groups contribute expected source bytes based on real
   IP total lengths.
4. Unrecoverable groups contribute adaptive FEC pressure only.

FEC health no longer commits QoS limited state by itself. It can raise
`fecRepairCount`, which improves future recovery probability. Once future groups
become recoverable, recovered source bytes let the rate estimator make a
rate-based QoS decision.

This preserves the design goal that QoS is rate-based while still handling
missing samples.

## Adaptive FEC Policy

The receiver tracks group outcomes per session and lane:

```text
observed groups
complete groups
recovered groups
unrecoverable groups
missing DATA packet count
received REPAIR packet count
```

The repair count is driven by FEC health/loss ratio. In this design, that means
packet-level group loss pressure, not byte-level missing size, because
unrecoverable groups do not reveal missing DATA byte lengths.

FEC health is the DATA packet arrival ratio observed from FEC group outcomes:

```text
fecHealth = sum(DataArrived) / sum(DataExpected)
```

The adaptive policy converts FEC loss ratio into a target lane repair count.
For the current full group size, `K = 4`:

```text
lossRatio = (sum(DataExpected) - sum(DataArrived)) / sum(DataExpected)
targetRepairCount = ceil(lossRatio * K)
repairCount = clamp(targetRepairCount, 1, 4)
```

Examples:

```text
DataArrived=3, DataExpected=4
lossRatio = 25%
repairCount = ceil(0.25 * 4) = 1
=> 4+1

DataArrived=2, DataExpected=4
lossRatio = 50%
repairCount = ceil(0.50 * 4) = 2
=> 4+2

DataArrived=1, DataExpected=4
lossRatio = 75%
repairCount = ceil(0.75 * 4) = 3
=> 4+3

DataArrived=0, DataExpected=4
lossRatio = 100%
repairCount = ceil(1.00 * 4) = 4
=> 4+4
```

This formula controls only adaptive FEC. It does not directly commit QoS
limited or clear state.

The output of the adaptive policy is only:

```text
repairCount = 1..4
```

`repairCount = 1` is the default `4+1` behavior. Higher values request more
REPAIR frames per future group.

## End-To-End Flow

### Healthy Path

```text
sender DATA group 4 packets
sender emits 1 REPAIR
receiver receives all DATA or recovers at most 1 missing DATA
receiver computes expected source bytes from IP packet lengths
estimator remains rate-based
LINK_STATUS keeps repairCount = 1
```

### Severe Loss Path

```text
sender emits 1 REPAIR
receiver sees many groups with 2+ missing DATA packets
groups are unrecoverable
receiver does not synthesize missing DATA bytes
adaptive FEC raises repairCount from FEC health/loss ratio
QoS writer sends LINK_STATUS with repairCount bits increased
sender applies lane-local fecRepairCount
future groups get multiple REPAIR frames
receiver recovers more groups
receiver computes expected source bytes from actual IP packet lengths
estimator gets rate-based expected-vs-actual observations
```

### QoS and FEC Interaction

```text
FEC health -> adaptive repair count
recovered packets -> expected source bytes
expected source bytes vs actual DATA bytes -> QoS estimator
QoS estimator -> limited/clear LINK_STATUS bits
LINK_STATUS repair bits -> sender FEC count
```

FEC health is upstream of adaptive FEC, not a direct QoS decision.

## Module Boundaries

Public architecture boundaries remain unchanged:

- Protocol encodes and decodes REPAIR and LINK_STATUS.
- FEC exposes shard-level `Encode` and `Reconstruct`.
- Send owns lane-local FEC transmit windows and repair emission.
- Recv owns lane-local receive windows, recovery, estimator inputs, and local
  QoS/FEC feedback.
- Runtime QoS writer converts Recv feedback into LINK_STATUS frames.
- Runtime RecvHandler decodes inbound LINK_STATUS and calls the existing lane
  QoS input with QoS bits plus `repairCount`.
- The lane QoS input updates selector quality and calls the unexported
  lane-internal `setFEC(repairCount)`.
- Session does not know lanes, FEC, protocol frames, or transports.
- Transport works with bytes and Go network primitives only.

## Migration Plan

Implement in layers so each layer has direct tests:

1. FEC codec supports `repairShards > 1` and `keys []uint16`.
2. Protocol LINK_STATUS status validation accepts the new nibble layout.
3. Send lane state stores `fecRepairCount` and emits multiple REPAIR frames.
4. Recv window stores multiple REPAIR shards per group and reconstructs multiple
   missing DATA packets.
5. Recv group observations use actual IP lengths for expected source bytes.
6. Estimator stops using repair symbol size as source-byte input.
7. QoS writer encodes repair count into LINK_STATUS.
8. RecvHandler decodes repair count and passes it through lane QoS input;
   `laneQoSInput` calls lane-local `setFEC`.
9. Adaptive policy computes repair count from group loss pressure.

Each layer should preserve existing `4+1` behavior when repair count is `1`.

## Non-Goals

This design does not add:

- tunnel-level retransmission;
- reliable delivery semantics;
- source-symbol fragmentation;
- fixed-size FEC symbols independent of IP packet boundaries;
- size-bucketed FEC grouping;
- public Lane or Path modules;
- target fields for FEC control;
- transport-kind-scoped FEC transmit state;
- cross-leg capacity heuristics for QoS decisions.

## Tests

Codec tests:

- `4+1` existing one-missing behavior still works.
- `4+2` recovers two missing DATA shards.
- `4+3` recovers three missing DATA shards.
- mixed DATA sizes produce REPAIR shards at max group DATA length.
- recovered mixed-size DATA is returned padded for the caller to truncate.
- duplicate keys are deduped before reconstruction.

Protocol tests:

- LINK_STATUS accepts nibble values for repair counts `1..4`.
- LINK_STATUS encodes the valid state values `0000..0111`.
- QoS bit extraction remains correct while repair bits are set.

Send tests:

- default lane repair count emits one REPAIR.
- repair count `N` emits `N` REPAIR frames for one group.
- generated REPAIR frames share group identity and carry distinct keys.
- REPAIR frames continue to use the lane shadow role.
- DATA buffers are released after all REPAIR symbols are encoded.

Recv tests:

- multiple REPAIR frames for one group do not overwrite each other.
- two missing DATA packets plus two independent REPAIR packets recover.
- recovered DATA is emitted once and marked as recovered, not DATA-leg wire.
- unrecoverable groups do not synthesize missing source bytes.
- duplicate REPAIR keys do not count as independent equations.

Estimator tests:

- expected source bytes use actual and recovered IP total lengths.
- `source_span * repair_symbol_size` is not used as source-byte input.
- FEC health does not directly commit QoS limited state.
- unrecoverable groups affect adaptive repair count, not expected bps.

Runtime tests:

- QoS writer encodes the same repair count into both status nibbles.
- RecvHandler decodes repair count and passes it through `QoSInput.OnQoSStatus`.
- `laneQoSInput.OnQoSStatus` calls lane-internal `setFEC`.
- QoS limited state and repair count changes can be emitted in one LINK_STATUS.

Integration tests:

- repair count `1` preserves current `4+1` behavior.
- repair count `2` recovers groups with two missing DATA packets.
- under severe loss, adaptive repair count increases and future groups produce
  multiple REPAIR frames.
- after multi-repair recovery, QoS estimator receives accurate expected source
  bytes derived from IP packet lengths.

Remote live E2E should verify:

- LINK_STATUS carries repair-count changes;
- sender applies lane-local repair count;
- REPAIR frame count per group increases under loss;
- recovered group byte accounting matches IP packet lengths;
- QoS state changes remain rate-based.
