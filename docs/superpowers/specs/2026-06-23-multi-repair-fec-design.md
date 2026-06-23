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

The REPAIR frame wire layout does not change:

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

### LINK_STATUS

`LINK_STATUS.status` remains the unified feedback byte. It carries both QoS
limited state and adaptive FEC repair count:

```text
status = UUUU TTTT

high nibble = UDP snapshot
low nibble  = TCP snapshot

nibble bit 0    = QoS limited
nibble bits 1-3 = repairCount - 1
```

Initial valid repair counts are `1..4`:

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

Outbound LINK_STATUS duplicates the lane-level repair count into both nibbles.
QoS limited bits remain per transport kind.

Example:

```text
UDP limited = true
TCP limited = false
repairCount = 3

UDP nibble = 0101
TCP nibble = 0100
status     = 0101 0100
```

Inbound LINK_STATUS applies the QoS bits to the selector as today and applies
the decoded repair count to the matching lane's FEC transmit state. If the two
nibbles carry different repair counts, the receiver of the status applies the
larger count and logs the mismatch. This keeps the sender conservative if a
future peer sends inconsistent snapshots.

LINK_STATUS is emitted when either committed QoS state changes or the requested
lane repair count changes. Delivered-bps fields remain auxiliary snapshot data;
they are not a continuous telemetry stream.

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

1. Accept variable-length DATA shards.
2. Build internal equal-length work buffers using the maximum known shard
   length.
3. Generate one coefficient row per key using the existing TinyMT coefficient
   generator.
4. Use `github.com/klauspost/reedsolomon` with a custom matrix for encode and
   reconstruct.
5. Return recovered DATA shards at coded shard length.
6. Leave IP total length truncation to Recv.

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

### Emission Flow

When `Send.Write` selects a lane and writes a DATA frame successfully:

```text
TUN packet
 -> scheduler picks lane
 -> DATA written on lane primary role
 -> DATA packet copied into that lane's FEC transmit window
```

When the transmit window produces a group:

1. Read the lane's current `fecRepairCount`.
2. Compute `repair_symbol_size = max(len(DATA[i]))`.
3. Build source shards for FEC coding.
4. Allocate `fecRepairCount` unique REPAIR keys.
5. Build `repairShards = fecRepairCount`.
6. Call `Codec.Encode(shards, keys)`.
7. Emit one REPAIR frame per repair shard with the matching key.
8. Send each REPAIR through the lane shadow role.
9. Release the buffered DATA packets after all repair symbols are encoded.

Partial groups created by the existing flush path use the same repair count.
Their `source_span` is the number of protected DATA packets in that partial
group.

### Applying LINK_STATUS

The runtime handler decodes `LINK_STATUS.status` and delivers:

- UDP limited state and UDP delivered bps to selector quality;
- TCP limited state and TCP delivered bps to selector quality;
- decoded lane repair count to the lane's FEC transmit state.

The selector still decides DATA and REPAIR roles. FEC count only controls how
many REPAIR frames are emitted for a completed group.

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

The receive window stores group state by:

```text
group key = (base_packet_id, source_span)
repair key = key carried by the REPAIR frame
```

Each group tracks:

- received DATA shards;
- received REPAIR shards keyed by `key`;
- which DATA shards arrived from the DATA leg;
- which DATA shards were recovered by FEC;
- DATA wire bytes from original DATA arrivals;
- REPAIR wire bytes from REPAIR arrivals;
- group completion/recovery state.

The current "one repair per base packet id" storage is replaced by "many repair
shards per group".

### Recovery Flow

On DATA arrival:

1. Drop duplicates before touching FEC or QoS bookkeeping.
2. Store the DATA shard in the lane receive window.
3. Mark it as DATA-leg wire delivery.
4. Try recovery for any live group containing this packet.

On REPAIR arrival:

1. Validate session, lane, source span, and group bounds.
2. Store the REPAIR shard if its key is new for the group.
3. Ignore duplicate repair keys for the same group.
4. Add its payload length to REPAIR wire bytes.
5. Try recovery for the group.

Recovery is attempted when:

```text
known DATA shards + known REPAIR shards >= source_span
and at least one DATA shard is missing
```

The window builds a compact shard set from all received DATA shards and enough
received REPAIR shards, creates a codec with `repairShards = selected repair
count`, and calls `Reconstruct(shards, keys)`.

After successful recovery:

1. Parse each recovered DATA shard as an IP packet.
2. Read IP total length.
3. Truncate the recovered shard to the IP total length.
4. Emit recovered DATA to TUN only if it has not already been emitted.
5. Mark recovered DATA as recovered source bytes, not DATA-leg delivery.
6. Close the group and produce a group observation for the estimator.

If a group expires before recovery, the receiver records packet-count loss
pressure for adaptive FEC. It does not synthesize missing source bytes.

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

The receiver tracks group outcomes per session and lane over a bounded rolling
window:

```text
observed groups
complete groups
recovered groups
unrecoverable groups
missing DATA packet count
received REPAIR packet count
```

The initial policy derives a requested repair count from observed packet-loss
pressure:

```text
missing_per_group = missing DATA packet count / observed groups
smoothed_missing_per_group = EMA(missing_per_group)

if smoothed_missing_per_group < 0.25:
  requestedRepairCount = 1
else:
  requestedRepairCount = clamp(1 + ceil(smoothed_missing_per_group), 1, 4)
```

Examples:

```text
mostly healthy:    smoothed_missing_per_group = 0.1 -> repairCount 1
one missing often: smoothed_missing_per_group = 1.0 -> repairCount 2
two missing often: smoothed_missing_per_group = 2.0 -> repairCount 3
three missing often: smoothed_missing_per_group = 3.0 -> repairCount 4
```

To avoid oscillation, the policy applies smoothing and hysteresis:

- increases may happen quickly after sustained unrecoverable pressure;
- decreases require a longer healthy window;
- repair count changes are emitted only when the committed count changes.

This policy is intentionally based on packet loss pressure, not byte estimates,
because unrecoverable groups do not reveal missing DATA byte lengths.

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
adaptive policy raises requested repairCount
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
- Runtime RecvHandler applies inbound LINK_STATUS to Send's lane QoS/FEC input.
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
8. RecvHandler decodes repair count and applies it to lane-local Send FEC.
9. Adaptive policy computes repair count from rolling group loss pressure.

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
- LINK_STATUS rejects reserved repair count values.
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
- RecvHandler decodes repair count and applies it to lane-local Send FEC.
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
