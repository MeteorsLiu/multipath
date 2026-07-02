# Multipath Tunnel Protocol

This document describes the current v2 wire protocol for the multipath tunnel.

## Scope

This protocol is a multipath tunnel protocol. It carries complete IP packets
between TUN interfaces across multiple logical lanes. It is not an end-to-end
reliable transport protocol.

The protocol owns:

- session identification
- lane identification
- UDP-first transport with per-lane TCP fallback
- keepalive and path health frames
- optional SLC forward erasure correction

The protocol does not own:

- ACK-driven retransmission
- global packet ordering
- stream flow control
- transport-level reliability

With FEC enabled, the receiver keeps bounded per-lane SLC window bookkeeping
keyed within that lane by `packet_id` and `base_packet_id`. That bookkeeping
exists only to recover lost tunnel packets and avoid emitting a recovered packet
twice if the original arrives late. It is not a general reliable-transport
deduplication layer.

## Terminology

- Session: one logical tunnel between a client and a server.
- Lane: one independently scheduled logical path inside a session.
- Transport leg: the concrete carrier for a lane, currently UDP or TCP.
- Fallback: a per-lane transport decision. A lane may keep a warm TCP leg while
  still sending DATA on UDP, then use TCP when UDP is unavailable or selected
  against due to lane quality.
- Frame: one protocol message after the UDP or TCP transport envelope.
- SLC: the protocol's sliding linear coding layer. SLC profiles use random
  linear coding over GF(2^8) with DATA shards and one REPAIR shard.
- DATA symbol: the FEC-protected representation of one tunnel IP packet.
- REPAIR symbol: one FEC parity symbol generated from DATA symbols.
- packet_id: a session-scoped DATA identifier used by each lane-local SLC
  window.
- base_packet_id: the first DATA identifier protected by one lane-local REPAIR
  group.
- key: the SLC coding key used to derive the linear coefficients for a REPAIR
  symbol.

## Encoding

All integer fields use network byte order.

UDP carries exactly one frame per UDP datagram:

```text
Frame
```

TCP is a byte stream, so every frame is length-prefixed:

```text
frame_len  uint16
Frame      bytes[frame_len]
```

`frame_len = 0` is invalid. A receiver drops the TCP connection if it observes
an invalid length or truncated frame.

## Common Frame Header

Every frame begins with the same 10-byte header:

```text
0       1                       9       10
+-------+-----------------------+-------+
| vt    | session_id            | lane  |
| 1B    | 8B                    | 1B    |
+-------+-----------------------+-------+
| type-specific body ...                |
+---------------------------------------+
```

Fields:

```text
vt          uint8   // high 4 bits = version, low 4 bits = type
session_id uint64  // opaque random session id
lane_id    uint8   // logical scheduling lane
body       bytes
```

`session_id` is intentionally 64 bits, not 128 bits, to keep per-packet tunnel
overhead small while preserving a large random active-session space. It is a
session discriminator, not an authentication token. A receiver treats a
collision in its active session table as a failed HELLO and requires the peer
to retry with a new session id.

`lane_id` is intentionally 8 bits because it is scoped to one session. A
session with more than 254 active lanes is outside the intended design. The
value `0xff` is reserved for session-scoped control frames such as
session-level CLOSE.

## MTU Calculation

The TUN MTU must be chosen so that tunnel DATA frames and, when enabled, REPAIR
frames do not force outer IP fragmentation. The calculation uses the outer L3
path MTU. It does not include an Ethernet header, and the TUN packet is a
complete IP packet without a layer-2 header.

Protocol overhead:

```text
common header = 10 bytes
DATA body     = 4 bytes packet_id
DATA overhead = 14 bytes

REPAIR frame header = 10 bytes common header
                    + 4 bytes base_packet_id
                    + 2 bytes key
                    + 1 byte source_span
                    = 17 bytes

REPAIR-protected IP packet overhead = 17 bytes
```

For UDP:

```text
udp_payload_budget = path_mtu - outer_ip_header - udp_header
udp_data_tun_mtu   = udp_payload_budget - 14
udp_fec_tun_mtu    = udp_payload_budget - 17
```

For TCP:

```text
tcp_payload_budget = effective_tcp_mss
tcp_data_tun_mtu   = tcp_payload_budget - 2 - 14
tcp_fec_tun_mtu    = tcp_payload_budget - 2 - 17
```

The extra `2` bytes for TCP are the `frame_len` prefix.

Operationally, round the calculated TUN MTU down to a multiple of 10:

```text
aligned_tun_mtu = floor(calculated_tun_mtu / 10) * 10
```

For v2, only IPv4 underlay is considered. With `path_mtu = 1500` and a minimum
TCP header:

```text
UDP payload budget = 1500 - 20 - 8  = 1472
TCP payload budget = 1500 - 20 - 20 = 1460

UDP, FEC off       = 1472 - 14 = 1458 -> 1450 aligned
UDP, FEC on        = 1472 - 17 = 1455 -> 1450 aligned

TCP, FEC off       = 1460 - 2 - 14 = 1444 -> 1440 aligned
TCP, FEC on        = 1460 - 2 - 17 = 1441 -> 1440 aligned
```

Recommended TUN MTU is the minimum value across all enabled transport legs and
enabled protocol features after alignment:

```text
UDP only + FEC = 1450
TCP only + FEC = 1440
UDP/TCP auto fallback + FEC = 1440
```

If TCP options reduce the effective TCP MSS, use the actual MSS in the formula
instead of assuming `1460`.

Version:

```text
2 = this protocol draft
```

Frame types:

```text
0x1 HELLO
0x2 HELLO_ACK
0x3 PING
0x4 PONG
0x5 DATA
0x6 REPAIR
0x7 CLOSE
0x8 BW_PROBE
0x9 BW_PROBE_ACK
0xa LINK_STATUS
```

`session_id` identifies the tunnel. `lane_id` identifies the lane that carries
this frame and is used for scheduling, health tracking, and per-lane fallback.
FEC transmit and receive windows are lane-local: DATA and REPAIR frames
participate in the same SLC scope only when they have the same `session_id` and
`lane_id`. `packet_id` remains session-scoped, and `base_packet_id` identifies
the protected group inside that lane-local SLC window.

Unknown versions or frame types are dropped. On TCP, repeated invalid frames
should close the transport leg.

## Capability Bits

`caps` is a `uint16` bitset used in HELLO and HELLO_ACK:

```text
bit 0 = TCP fallback supported
bit 1 = FEC supported
bit 2 = LINK_STATUS supported
```

`fec_profile` is a `uint8`:

```text
0 = off
1 = slc_4_plus_1
2 = slc_variable_plus_1
```

Both peers use the capability intersection returned in HELLO_ACK. If both peers
support FEC, the negotiated `fec_profile` is the highest profile supported by
both peers. LINK_STATUS is meaningful only with FEC enabled because its QoS
evidence comes from DATA/REPAIR group observations.

## Type 0x1: HELLO

HELLO establishes or refreshes one lane over one transport leg.

Body:

```text
nonce       uint64
caps        uint16
fec_profile uint8
```

Sender behavior:

1. Choose or reuse the session id for the tunnel.
2. Choose the lane id for this configured path.
3. Send HELLO on the transport leg being established.
4. Retry HELLO until HELLO_ACK arrives or the attempt times out. TCP fallback
   HELLO timeout should use an RTO-style budget derived from existing RTT
   estimates when available; UDP HELLO may use the probe timeout policy.

Receiver behavior:

1. Validate version and body length.
2. If the session does not exist, create it only if this endpoint accepts new
   sessions from this peer.
3. If the lane does not exist, create the lane under the session.
4. Bind or refresh this transport leg for `(session_id, lane_id)`.
5. Record the observed remote address for UDP.
6. Reply with HELLO_ACK.

UDP-specific rule:

The receiver must reply from the same UDP socket that received HELLO. The
destination address is the observed source address of the HELLO datagram. This
is required for NAT and conntrack friendliness.

Duplicate HELLO:

HELLO is idempotent. A duplicate HELLO refreshes the same lane and receives a
new HELLO_ACK with the same nonce echoed.

## Type 0x2: HELLO_ACK

HELLO_ACK confirms whether a lane transport leg is accepted.

Body:

```text
nonce       uint64 // echoed HELLO nonce
accepted    uint8  // 1 = accepted, 0 = rejected
caps        uint16 // negotiated capability intersection
fec_profile uint8
```

Sender behavior:

1. Echo the HELLO nonce.
2. Set `accepted = 1` when the session and lane are accepted.
3. Set `accepted = 0` when the session, lane, transport, or capability set is
   rejected.
4. Return the negotiated capability intersection.

Receiver behavior:

1. Validate version, body length, and nonce.
2. If `accepted = 0`, mark this transport attempt as failed.
3. If accepted over UDP, mark this lane's UDP leg active.
4. If accepted over TCP, mark this lane's TCP leg active.
5. Start or refresh keepalive for this lane.

## Type 0x3: PING

PING probes liveness and keeps NAT mappings warm. PING/PONG are small control
frames and are not a UDP data-plane bandwidth or QoS probe.

Body:

```text
ping_id uint64
time_ms uint64 // sender timestamp in milliseconds
```

Sender behavior:

1. Send PING periodically on every active transport leg.
2. Continue low-rate UDP PING even while the lane is using TCP fallback, so UDP
   recovery can be detected.
3. Track outstanding `ping_id` values per lane and per transport leg.

Receiver behavior:

1. Validate session and lane.
2. Update the lane and transport leg last-seen time.
3. Reply with PONG on the same transport leg.

## Type 0x4: PONG

PONG responds to PING and allows RTT estimation.

Body:

```text
ping_id uint64 // echoed PING ping_id
time_ms uint64 // echoed PING time_ms
```

Sender behavior:

1. Echo the PING body.
2. Send PONG on the same transport leg that received PING.

Receiver behavior:

1. Match `ping_id` to an outstanding PING.
2. Compute RTT from the echoed `time_ms`.
3. Update the lane and transport leg health bookkeeping.
4. If UDP becomes healthy while TCP fallback is active, move the lane back to
   UDP according to the lane health rules.

## Type 0x5: DATA

DATA carries one complete IP packet read from TUN.

Body:

```text
packet_id uint32
ip_packet bytes
```

`packet_id` is a session-wide monotonically increasing DATA identifier. It
exists for SLC window mapping only. It is not a delivery sequence number, and
the receiver must not wait for missing `packet_id` values before writing DATA
to TUN.

`packet_id` maps a DATA frame into the SLC repair window. For the fixed 4+1
profile, the source-symbol position is `packet_id - base_packet_id` inside a
REPAIR window, and valid positions are `0..3`. For the variable profile, valid
positions are `0..source_span-1`.

`packet_id` should not wrap inside one live session once session rotation is
defined. The exact exhaustion threshold and graceful rotation behavior are an
open item in this draft. Until that policy exists, implementations must not
turn packet-id exhaustion into a local fail-closed data-plane stop.

Sender behavior:

1. Read one complete IP packet from TUN.
2. Allocate the next session-wide `packet_id`.
3. Build DATA with `packet_id` and the IP packet.
4. Select a healthy lane.
5. Select that lane's transport leg using the send-side leg policy.
6. Prefer UDP when it is active and not selected against by quality policy.
7. Send DATA on the TCP leg when UDP is unavailable or the leg policy selects
   TCP.
8. Do not select a lane with no usable transport leg.
9. If the DATA frame is written successfully and FEC is enabled, insert
   `(packet_id, ip_packet)` into the SLC transmit window.

Receiver behavior:

1. Validate session and lane.
2. Update the lane and transport leg last-seen time.
3. Save `(packet_id, ip_packet)` into the bounded SLC receive window if FEC is
   enabled.
4. Write `ip_packet` to TUN immediately.

The receiver does not reorder DATA and does not block waiting for gaps.

## Type 0x6: REPAIR

REPAIR carries one SLC repair symbol used to recover lost DATA symbols.

Body:

```text
base_packet_id                uint32
key                           uint16
source_span_and_repair_count  uint8
repair_symbol                 bytes[repair_symbol_size]
```

`source_span_and_repair_count` is a packed byte:

```text
bits 0-2 = source_span
bits 3-4 = repair_count - 1
bits 5-7 = reserved, must be zero
```

`source_span` is the decoded number of contiguous DATA symbols protected by this
REPAIR frame. It defines the protected range:

```text
base_packet_id .. base_packet_id + source_span - 1
```

Rules:

```text
fec_profile = slc_4_plus_1:
  source_span = 4

fec_profile = slc_variable_plus_1:
  source_span = 1..4
```

For both SLC profiles:

```text
repair count    = 1..4
field           = GF(2^8)
```

`repair_count` is the total number of REPAIR frames the sender generated for
this FEC group. Every REPAIR frame for the same group must carry the same
decoded `repair_count`.

The protected source symbols are:

```text
base_packet_id
base_packet_id + 1
...
base_packet_id + source_span - 1
```

The DATA symbol used for FEC is the IP packet itself:

```text
ip_packet bytes
```

DATA frames never carry padding. For repair calculation only, packets shorter
than the repair symbol length are treated as if bytes beyond `len(ip_packet)`
were zero. This is a virtual zero extension, not bytes sent on the wire.

The repair symbol length is the largest IP packet length in the protected repair
window:

```text
repair_symbol_size = max(len(P0), ..., len(P[source_span-1]))
```

The `4` bytes are `base_packet_id`, the `2` bytes are `key`, and the `1` byte is
`source_span_and_repair_count`:

```text
repair_symbol_size = frame_body_len - 4 - 2 - 1
```

Repair calculation:

```text
repair_symbol[i] =
  coeff[0] * source[0][i] +
  coeff[1] * source[1][i] +
  ... +
  coeff[source_span-1] * source[source_span-1][i]
```

The arithmetic is over `GF(2^8)` using the irreducible polynomial
`x^8 + x^4 + x^3 + x^2 + 1`. Coefficients are generated deterministically from
`key` using the RFC 8681 RLC coefficient generation function with TinyMT32 from
RFC 8682. This protocol fixes `DT = 15`, so all coefficients are nonzero.

Sender behavior for `slc_4_plus_1`:

1. After a DATA frame is written successfully, add its DATA symbol to that
   lane's SLC transmit window.
2. After four contiguous successfully written DATA symbols, compute the
   configured number of REPAIR symbols for this group.
3. Set `base_packet_id` to the first protected DATA symbol.
4. Increment or otherwise vary `key` for each REPAIR.
5. Encode `source_span = 4` and the group `repair_count` in
   `source_span_and_repair_count`.
6. Set the repair symbol length to the largest IP packet length in this repair
   window.
7. Compute the REPAIR symbol with coefficients derived from `key`.
   Source bytes beyond the end of a shorter IP packet are treated as zero.
8. Send REPAIR on the same lane's shadow transport role.

If fewer than four contiguous DATA symbols are available, `slc_4_plus_1` does
not emit REPAIR.

Sender behavior for `slc_variable_plus_1`:

1. After a DATA frame is written successfully, add its DATA symbol to that
   lane's SLC transmit window.
2. Arm a flush timer when the pending SLC transmit window transitions from zero
   symbols to one symbol.
3. If four contiguous successfully written DATA symbols become available before
   the timer fires, cancel the timer and emit the configured number of full
   REPAIR symbols with `source_span = 4`.
4. If the timer fires with one to three contiguous DATA symbols pending, emit
   the scaled number of partial REPAIR symbols with `source_span` set to the
   number of protected symbols.
5. Set `base_packet_id` to the first protected DATA symbol.
6. Increment or otherwise vary `key` for each REPAIR.
7. Encode the decoded `source_span` and group `repair_count` in
   `source_span_and_repair_count`.
8. Set the repair symbol length to the largest IP packet length in this repair
   window.
9. Compute the REPAIR symbol with coefficients derived from `key`.
   Source bytes beyond the end of a shorter IP packet are treated as zero.
10. Send REPAIR on the same lane's shadow transport role.

For `slc_variable_plus_1`, the flush interval is derived from the sender's
per-leg RTT estimator:

```text
flush_ms = clamp(max_session_srtt * alpha, min_ms, max_ms)
```

If no RTT sample exists for the session, the sender uses a configured
cold-start RTT value in the same formula. A configured fixed flush interval may
override the RTT-derived value for deterministic tests.

Receiver behavior:

1. Validate session and lane.
2. Decode `source_span_and_repair_count`; reject frames with nonzero reserved
   bits, invalid `source_span`, or invalid `repair_count`.
3. Validate decoded `source_span` for the negotiated `fec_profile`.
4. Use `lane_id`, `base_packet_id`, and decoded `source_span` to identify the protected
   DATA symbols in that lane's SLC receive window.
5. Store the REPAIR symbol and decoded `repair_count` while the protected
   lane-local window is still alive. If REPAIR frames for the same group carry
   inconsistent `repair_count`, FEC recovery may still use the received symbols
   but that group is not trustworthy as a REPAIR-side QoS expectation sample.
6. If all protected DATA symbols are already known, drop the REPAIR.
7. If exactly one protected DATA symbol is missing, recover it using the REPAIR
   symbol and coefficients derived from `key`.
8. If multiple protected DATA symbols are missing, keep the REPAIR until more
   DATA arrives or the window expires. A single SLC REPAIR cannot recover two
   missing DATA symbols.
9. For each recovered DATA symbol, parse the IP header to obtain the packet
   total length, truncate to that length, and write the recovered IP packet to
   TUN if it has not already been emitted.

The receiver keeps bounded DATA and REPAIR bookkeeping. Expired or evicted
symbols are not retransmitted.

## Type 0x7: CLOSE

CLOSE shuts down a lane or the whole session.

Body:

```text
scope  uint8 // 1 = lane, 2 = session
reason uint8
```

Reason values:

```text
1 = unknown_session
```

Sender behavior:

1. Use `scope = 1` to close the current `lane_id`.
2. Use `scope = 2` to close the entire `session_id`.
3. Send CLOSE on any active transport leg for the target scope.

Receiver behavior:

1. Validate session.
2. If `scope = 1`, close the specified lane and clean its transport resources.
3. If `scope = 2`, close the whole session and clean all lanes, transport legs,
   keepalive bookkeeping, and FEC windows.
4. If `reason = unknown_session`, a client with configured bootstrap lanes
   creates a fresh local session id and reopens those lanes with HELLO.

For session CLOSE, `lane_id = 0xff` may be used when no specific lane is
intended.

## Type 0x8: BW_PROBE

BW_PROBE is an optional data-plane-sized bandwidth probe frame. It is not
emitted to TUN and is not part of FEC.

Body:

```text
train_id              uint64
probe_id              uint64
seq                   uint16
count                 uint16 // total frames in this probe round, 1..64
send_ms               uint64
target_bps            uint64
train_bytes_remaining uint64
payload               bytes
```

`target_bps` is the sender's effective target/cap rate for this train. When a
configured cap is present, it carries that cap. When no configured cap is
present, it may carry the measured or configured reference used as the effective
cap for this train. A value of zero means no target was provided.
`train_bytes_remaining` is the sender's local byte budget remaining after this
frame. A value of zero marks normal train completion.

Sender behavior:

1. Start bandwidth-probe ramps only for transport legs selected by the
   send-side leg quality policy.
2. Use `seq = 0..count-1` inside one `probe_id`.
3. Pace probe frames according to the current probe rate. Do not send a large
   unpaced burst. UDP probe payloads should remain data-plane/MTU sized; TCP
   probe payloads may be larger stream frames so the ramp can put enough bytes
   in flight to measure stream capacity.
4. Increase the next probe rate only while the leg quality policy still needs
   more evidence for the current leg.
5. Run the probe as a sustained window. A short burst only measures transient
   delivery rate; it does not prove sustainable goodput on links with periodic
   shaping or stalls.
6. Do not keep sending periodic bandwidth probes after a leg's ramp completes.
   A new concrete leg may start a new ramp.
7. Serialize bandwidth trains globally per session. The order is client lane
   N, server lane N, client lane N+1, server lane N+1.
8. When no configured bandwidth cap is present, each lane first runs TCP as a
   reference and then UDP. When a cap is present, only UDP is probed.
   Application JSON uses `bandwidthProbeCapBps < 0` to request no cap; omitted
   or zero values use the application default cap.

Receiver behavior:

1. Validate session, lane, and body length.
2. Drop frames with `count = 0`, `count > 64`, or `seq >= count`.
3. Do not emit the payload to TUN.
4. Maintain a per-leg cumulative receive bitmap for the current probe round.
5. Reply with BW_PROBE_ACK on the same transport leg.
6. When `train_bytes_remaining = 0`, release the bandwidth-probe gate for the
   next local train phase. In the normal case this value is carried by an
   ordinary payload-bearing BW_PROBE frame as the train byte budget naturally
   reaches zero. If a train stops before naturally consuming its byte budget,
   the sender may send a zero-payload BW_PROBE with `train_bytes_remaining = 0`
   only as a gate-release signal.
7. If the BW_PROBE frame carrying `train_bytes_remaining = 0` is lost, release
   only the bandwidth-probe gate after an idle timeout of
   `clamp(8*SRTT, 500ms, 10s)`, falling back to the configured probe timeout
   when SRTT is unavailable. This timeout does not mark the lane or leg down.

## Type 0x9: BW_PROBE_ACK

BW_PROBE_ACK reports the receiver's cumulative bitmap for one bandwidth probe
round.

Body:

```text
probe_id    uint64
base_seq    uint16 // currently 0
count       uint16
received    uint64 // bit N set means seq base_seq+N was received
first_rx_ms uint64
last_rx_ms  uint64
```

Sender behavior:

1. Match `probe_id` to an outstanding bandwidth probe round.
2. Merge `received` into the round's cumulative ACK bitmap.
3. Finish the round early when the ACK bitmap covers all expected probe frames;
   otherwise finish it after the ACK timeout. Once ACK-delay SRTT exists, the
   ACK timeout is `min(4*SRTT, 1s)`. Before any ACK-delay sample exists, use an
   initial 500ms timeout.
4. Compute received count and loss for the round.
5. Feed the sample into per-leg bandwidth EWMA and leg selection policy.
6. When the lane has enough TCP-vs-UDP evidence to classify the leg quality,
   emit a lane-level decision describing whether UDP showed QoS/limit evidence,
   whether TCP measured better, and which leg should carry DATA.

Receiver behavior:

1. Validate session and lane.
2. Drop ACKs that do not match an outstanding local probe round.
3. Do not emit anything to TUN.

## Type 0xa: LINK_STATUS

LINK_STATUS carries receive-side QoS state for one lane as a UDP/TCP status
snapshot. It is derived from DATA and REPAIR observations; it is not a liveness
frame, does not mark a leg up or down, and is not periodic bandwidth telemetry.

Body:

```text
status            uint8   // high nibble = UDP state, low nibble = TCP state
udp_delivered_bps uint32
tcp_delivered_bps uint32
```

`status` is a complete lane snapshot:

```text
status = UUUU TTTT

high uint4 = UDP state
low uint4  = TCP state

each uint4:
  bit0    = QoS limited state
  bits1-3 = repairCount - 1
```

The default repair count is `1`, so repair bits `000` mean one REPAIR packet
per FEC group. Current valid per-transport state values are `0..7`.

`repairCount` is lane-local. The sender writes the same repair-count bits into
the UDP and TCP nibbles. The QoS bit remains transport-kind specific.
The encoded repair-count bits must match the receive-side committed QoS state;
the LINK_STATUS writer must not clamp, rewrite, or reset `repairCount` while
encoding the frame.

The `repairCount` bits are feedback to the peer sender for future FEC groups.
REPAIR frames carry the per-group repair count that the sender actually used.
The receiver must not assume that a newly sent LINK_STATUS repair count has
already affected the current receive-side group; it should use the decoded
REPAIR-frame repair count for that group. A receive group is recovered as soon
as the received REPAIR symbols are sufficient for its missing DATA symbols; the
receive window does not wait for every REPAIR symbol that the peer might have
sent.

Sender behavior:

1. Send LINK_STATUS only after both peers negotiated FEC and LINK_STATUS.
2. Send LINK_STATUS only from receive-side QoS state derived from DATA and REPAIR
   observations. Do not synthesize it from local ping timeout, TCP write error,
   or bandwidth-probe state alone.
3. Runtime QoSWriter sends LINK_STATUS through `Send.WriteFrame` with a TCP
   transport ref for the target lane. It does not delegate this frame to the
   lane's default control-transport policy.
4. Set `status` as a complete lane snapshot. The high nibble carries UDP state
   and the low nibble carries TCP state. Each nibble carries that transport
   kind's QoS bit and the lane-local repair-count bits.
5. Set `udp_delivered_bps` and `tcp_delivered_bps` to the receive-side rate
   estimates for each transport kind when available. Use `0` when the receiver
   has no estimate for that kind.
6. Send LINK_STATUS when the lane QoS state changes between clear and limited,
   or when the lane-local repair count changes. A delivered-bps estimate is
   auxiliary data in that state snapshot and is not a continuous telemetry
   stream. A delivered-bps-only change generally does not require another
   LINK_STATUS frame, except when both UDP and TCP are currently limited and
   the updated estimates change the QoS-preferred primary leg.

Receiver behavior:

1. Validate session, lane, and `status`.
2. Apply the QoS bits atomically to the matching lane selector quality.
3. Apply the decoded repair count to the matching lane's future FEC emission.
4. Keep the applied QoS and repair-count state until a later LINK_STATUS
   snapshot changes it.
5. Do not emit anything to TUN.

## Lane State Machine

Each lane owns independent UDP and TCP transport-leg health.

```text
new
  -> udp_probing
  -> udp_active
  -> tcp_warming
  -> udp_tcp_active
  -> tcp_selected
  -> closed
```

Important transitions:

```text
udp_probing + HELLO_ACK over UDP -> udp_active
udp_active + TCP HELLO sent      -> tcp_warming
tcp_warming + TCP HELLO_ACK      -> udp_tcp_active
udp_tcp_active + UDP selected    -> udp_tcp_active
udp_tcp_active + TCP selected    -> tcp_selected
tcp_selected + UDP selected      -> udp_tcp_active
open + CLOSE                     -> closed
```

Fallback and TCP warming are per lane. If lane A warms or selects TCP, lane B
can continue on UDP.

## Session Establishment Flow

Client creates one local session and one lane id per configured path. The
session id is generated by the Session module, not by runtime glue.

```text
Client lane 1 UDP -> Server: HELLO(session, lane=1)
Server lane 1 UDP -> Client: HELLO_ACK(session, lane=1)

Client lane 2 UDP -> Server: HELLO(session, lane=2)
Server lane 2 UDP -> Client: HELLO_ACK(session, lane=2)
```

After HELLO_ACK, each lane can carry DATA, REPAIR, PING, and PONG.

## Normal Data Flow

Example with two lanes and FEC enabled:

```text
TUN packet -> DATA packet_id=100 -> lane 1 UDP
TUN packet -> DATA packet_id=101 -> lane 1 UDP
TUN packet -> DATA packet_id=102 -> lane 1 UDP
TUN packet -> DATA packet_id=103 -> lane 1 UDP
             REPAIR base_packet_id=100 key=7 source_span=4 -> lane 1 shadow
TUN packet -> DATA packet_id=104 -> lane 2 UDP
```

`lane_id` on REPAIR identifies both the lane that carries the REPAIR frame and
the lane-local FEC receive window. A REPAIR frame protects only DATA symbols
sent with the same `(session_id, lane_id)`.

## UDP to TCP Warm Fallback Flow

Only the affected lane warms or selects TCP. TCP warming does not imply DATA is
sent on TCP; it only makes a TCP leg available for later leg selection and
bandwidth comparison.

```text
lane 2 UDP HELLO_ACK
Client opens lane 2 TCP leg in warm mode
Client -> Server over TCP: HELLO(session, lane=2)
Server -> Client over TCP: HELLO_ACK(session, lane=2)

lane 1 continues over UDP
lane 2 DATA and REPAIR continue using UDP unless leg policy selects TCP
```

If UDP becomes unavailable or the leg policy selects TCP:

```text
lane 2 DATA and REPAIR use TCP
UDP PING continues at low rate so UDP recovery remains observable
```

When UDP is selected again, DATA and REPAIR return to UDP. The TCP warm leg may
remain open for low-rate probing or be closed after a drain/cooldown policy.

## Receive-Side QoS Feedback

Receive-side QoS feedback requires FEC because the receiver needs DATA and
REPAIR observations to compare the selected DATA leg with the shadow leg. When
DATA and REPAIR arrive on the same transport kind, the observation is not
cross-leg evidence and must not produce a QoS decision.

The QoS estimator keeps only real receive-side sample classes:

```text
originalDataBytes = DATA bytes received without REPAIR and without late arrivals
expectedBytes     = source DATA bytes known from a completed or recovered FEC
                    group and IP packet header length parsing
repairBytes       = received REPAIR symbol bytes
lateDataBytes     = original DATA bytes that arrive after the same packet id
                    was recovered by FEC

periodic tick     -> convert pending sample classes to rate observations
                     and clear pending sample classes
```

For an already-observed DATA / REPAIR direction, an empty tick is still a
zero-byte rate observation. This lets rate state decay over time without
inventing DATA bytes, REPAIR bytes, or group samples.

Raw byte accounting is independent of FEC group lifetime. Accepted original
DATA adds `originalDataBytes` when it arrives. Received REPAIR adds
`repairBytes` when it arrives. Late DATA adds `lateDataBytes` when the
estimator has already seen that packet id as recovered. Only `expectedBytes`
is a FEC-group result; it is submitted when the group completes or recovers and
the receiver knows the source DATA byte total. A closed or dropped FEC group
must not decide whether raw DATA, REPAIR, or late-DATA bytes can be accounted.

Rate EMAs, PID correction, limited state, and the three-sample gap window are
scoped to the DATA/REPAIR direction and role. For example, `DATA=UDP,
REPAIR=TCP` is independent from `DATA=TCP, REPAIR=UDP`, and the primary/DATA
role is independent from the shadow/REPAIR role. The receiver records the
current primary DATA transport for the lane; the shadow transport is the
opposite kind. The initial receiver-side primary is UDP, matching the sender's
default selection. The receiver changes this primary only after the current
role's QoS tick commits a clear/limited state; it must not infer a primary
switch from an individual DATA arrival. The receive-side QoS tick is one second.
The estimator stores each tick's `rateGap`, averages three consecutive tick
gaps, and compares that three-sample average with the QoS thresholds. Do not add
a separate minimum group-count gate before evaluating QoS. DATA or REPAIR
observations that do not match the current primary/shadow pair are ignored by
the QoS estimator. Each tick evaluates only the current role state. Non-current
role state does not consume the tick or emit LINK_STATUS QoS state.

Before changing the current primary DATA direction, the receiver resets the
target primary/DATA state so stale actual or expected rate history cannot carry
into the new primary leg. It also resets the old primary's shadow/REPAIR state
so stale repair history cannot carry into shadow observation.
The receiver aggregates committed direction-local role state into the final
per-transport LINK_STATUS snapshot; the aggregated UDP/TCP status must not be
used as a gate for another direction's QoS judgment.

Duplicate DATA rejected by the session emit dedupe is dropped before TUN output
and must not contribute to `originalDataBytes`, FEC health, recovery, or emit
state, and it must not be inserted into the receive FEC window. The receiver's
QoS estimator may account that DATA once as `lateDataBytes` when its
recovered-packet state has seen the packet id. Duplicate original DATA that was
already emitted as original DATA is not late DATA. The same DATA frame must not
count twice, and FEC-recovered DATA must not be
converted into `originalDataBytes`, synthetic DATA bytes, mature rate samples,
or bandwidth-estimation inputs. Recovered IP lengths may be used only as
`expectedBytes` source bytes for a completed or recovered FEC group.

Unrecovered missing DATA may only contribute to FEC health observations:

```text
DataArrived / DataExpected
```

FEC health is not a direct QoS limited/clear detector input. It drives the
lane-local adaptive repair count carried in LINK_STATUS. That repair count has
two jobs:

1. When the current DATA primary is losing heavily, QoS rate detection may lack
   enough accepted DATA/group evidence. Higher repair count lets the shadow
   REPAIR leg recover more source packets sooner, restoring tunnel throughput
   and preserving receive-side group facts.
2. When the current DATA primary is backpressured and many originals arrive
   late, higher repair count lets the shadow REPAIR leg recover those packets
   before the delayed originals arrive, reducing upper-layer wait time.

The increased shadow REPAIR load is also the only valid shadow-capacity evidence
for clearing a previously limited transport kind. The receiver must not treat
the FEC-health value itself as a separate QoS clear signal.

Empty estimator ticks may decay rate EMAs, but they must not turn an old
incomplete-group health sample into QoS evidence. FEC health has separate
loss-health and late-health inputs. They must not be merged into one pressure
value or one EMA. Estimator ticks may clear the FEC-health dirty flag after
applying the current health state, but they must not reset the committed
`repairCount` except through explicit reset paths. A reset is allowed when the
primary protocol switches between UDP and TCP. A reset is also allowed when the
direction-local computed adaptive repair count remains at `4` for 75 seconds:
the receiver resets only that direction's FEC-health state to `repairCount=1`,
preserves QoS limited/clear state and rate windows, and lets subsequent loss or
late samples raise repair count again. The high-repair dwell timer starts when
the computed count reaches `4`, clears when it falls below `4`, and is reset by
primary switches. Any reset must happen before the LINK_STATUS snapshot is
emitted so the sent repair-count bits and the receiver's local committed state
remain identical.

Loss health is computed from group packet arrival ratio and reacts quickly to
rising loss:

```text
groupLossRatio = (DataExpected - DataArrived) / DataExpected
lossEMA        = EMA(lossEMA, groupLossRatio, 0.75 when rising, 0.05 when falling)
lossRepairCount = clamp(ceil(lossEMA * 4), 1, 4)
```

Late health is computed only by estimator ticks from that tick's own pending
byte counters. It is slower and more conservative than loss health:

```text
lateRatio   = lateDataBytes / expectedBytes
lateEMA     = EMA(lateEMA, lateRatio, 0.20 when rising, 0.05 when falling)

lateRepairCount = 1 when lateEMA <= 0.50
lateRepairCount = 2 when lateEMA <= 0.75
lateRepairCount = 3 when lateEMA <= 1.00
lateRepairCount = 4 when lateEMA >  1.00
```

If `expectedBytes` is zero for the tick, late health is not updated because the
tick has no DATA-load denominator. The committed adaptive repair count is:

```text
repairCount = max(lossRepairCount, lateRepairCount)
```

The receiver derives rate estimates from the real sample classes:

```text
originalDataBps = originalDataBytes / tick duration
expectedBps     = expectedBytes / tick duration
repairBps       = repairBytes / tick duration
lateDataBps     = lateDataBytes / tick duration
```

`expectedBps` is the DATA-side expectation from `expectedBytes`. It is not
`originalDataBytes + repairBytes`. The estimator does not maintain a fifth
pending byte class for derived repair expectations.

`lateDataBps` is retained as a separate late-arrival observation for logs and
late health. It must not be merged into `originalDataBytes` or directly mark a
leg limited/clear.

There are two limited-leg role paths:

1. DATA-leg limited: when `originalDataBps + lateDataBps` is materially below
   `expectedBps`, mark the DATA leg limited in the next LINK_STATUS snapshot.
2. Shadow-leg limited or clear: when source data is expected for the tick,
   compare `repairBps` with a REPAIR-specific expected rate. The expected rate
   is derived from completed groups. For each group:

   ```text
   groupRepairScale = maxSourceBytes / expectedBytes * repairCount
   ```

   where `maxSourceBytes` is the largest source DATA packet in that FEC group,
   `expectedBytes` is the complete source DATA byte total for that group, and
   `repairCount` is the decoded per-group REPAIR-frame repair count. The
   estimator updates a direction-local EMA with `groupRepairScale` when the
   group is submitted. Each tick then computes:

   ```text
   expectedRepairBps = expectedBps * repairScaleEMA
   deliveryGap       = rateGapRatio(expectedRepairBps, repairBps)
   loadGap           = rateGapRatio(expectedBps, repairBps)
   ```

   The limited decision uses the three-sample average of `deliveryGap`. The
   clear decision requires both three-sample averages to be below the clear
   threshold, or the current non-cap tick to show full shadow recovery with
   `repairBps >= expectedBps`:

   ```text
   deliveryGap <= 0.03
   loadGap     <= 0.10 OR repairBps >= expectedBps
   ```

   `deliveryGap` proves the peer-sent REPAIR load is delivered. `loadGap`
   proves that this REPAIR load is close to the DATA expectation. The clear
   threshold allows up to 10% measurement slack, so the shadow leg must carry at
   least 90% of the DATA expectation. A low-rate shadow trickle must not clear
   DATA-primary capacity: with default 4+1 FEC, one REPAIR for four source
   packets carries about 25% of `expectedBps`, so successful delivery of that
   default repair load is not enough recovery evidence. When a cap reference is
   active, cap-based clear still uses the cap load threshold rather than the
   `repairBps >= expectedBps` shortcut.

   REPAIR frames for the same group with inconsistent `repairCount` values make
   that group unusable for repair-scale updates.

This makes the feedback stable across a fallback transition:

```text
Initial:
  DATA = UDP
  REPAIR = TCP
  UDP DATA under-delivers
  -> receiver sends LINK_STATUS(status=UDP limited, TCP clear)

After selector switches DATA to TCP:
  DATA = TCP
  REPAIR = UDP
  TCP DATA is clean enough that adaptive repair count stays low
  UDP carries only low-rate default REPAIR
  -> receiver sends LINK_STATUS(status=UDP limited, TCP clear)

If TCP later loses heavily or backpressures DATA, FEC health raises the lane
repair count. Future groups then put higher REPAIR load on UDP. Only after UDP
delivers that higher REPAIR load within 10% of `expectedBps` may the receiver
send a clear LINK_STATUS for UDP and allow DATA to switch back.
```

Without the shadow-leg path, the UDP limited state would lose fresh state as
soon as DATA moves to TCP, even though UDP is still observable as the REPAIR
shadow leg.

If the receiver later sees the limited leg perform normally, it sends a new
LINK_STATUS snapshot with that transport kind clear. Clear and limited state are
represented in the same `status` byte.

## UDP QoS Bandwidth Probe Design

PING/PONG liveness does not reliably detect UDP QoS that targets large packets,
high packet rate, or sustained bandwidth. Implementations that need UDP QoS
detection should use a data-plane bandwidth probe separate from PING/PONG.

Recommended sender behavior:

1. Keep TCP warm for lanes that negotiated TCP fallback and have a TCP remote.
2. When no configured bandwidth cap is present, establish a TCP reference first,
   using a paced TCP BW_PROBE ramp or transport TCP_INFO where available. This
   reference is the comparison target for the lane, not a global configured
   maximum. When a bandwidth cap is configured, use that cap as the reference
   and do not run a TCP bandwidth-probe train.
3. Wait for that TCP reference only when the lane already has a TCP leg or has
   a configured TCP remote that this peer can dial. Negotiated fallback
   capability alone is not evidence that a TCP reference path exists.
4. Probe UDP after the TCP reference exists. UDP probing answers one question:
   can UDP get close enough to the TCP reference without material probe loss?
5. Increase UDP probe rate gradually toward the TCP reference while more
   evidence is needed. Do not continue increasing UDP just to discover its
   absolute ceiling after the relative TCP-vs-UDP decision is already clear.
6. Stop UDP probing as soon as either result is clear:
   UDP is close enough to the TCP reference with low loss, or UDP is materially
   below the TCP reference with loss or under-delivery evidence.
7. Record the completed paced step that produced the TCP reference or UDP
   decision into per-leg EWMA state. Do not average that result across the
   entire ramp from the minimum probe rate; the warmup ramp is control input,
   not the measured QoS sample.
8. Treat UDP as QoS-limited when the UDP sample has loss/under-delivery evidence
   and the active reference is materially better than UDP. In no-cap mode the
   active reference is TCP's measured bandwidth; in capped mode it is the
   configured bandwidth cap. Select TCP only when that material difference
   exists, so small probe differences do not override UDP preference.

The bandwidth probe is an initial capacity classification, not a continuous
monitor. A QoS decision for lanes with TCP fallback requires a UDP sample
relative to the active reference. No-cap mode uses a TCP reference sample;
capped mode uses the configured cap and does not wait for a TCP probe sample.
Once a leg's ramp completes, implementations should not clear the QoS-limited
state by periodic re-probing; a new concrete leg may be probed again.
BW_PROBE/BW_PROBE_ACK must not replace PING/PONG liveness or Session HELLO
state.

Known precision caveat: ACKs may arrive after the pacing step that sent their
BW_PROBE chunk. Implementations should attribute ACKed bytes to the step that
created the matching `probe_id`, not the step active when the ACK is processed.
Otherwise delayed ACKs can undercount one step, overcount the next step, and
make feedback pacing or peak-rate selection jitter.

## NAT and Conntrack Requirements

UDP is NAT-friendly only if the protocol uses the observed 5-tuple correctly.

Required behavior:

```text
client sends UDP HELLO to server
server replies to the observed source address
server replies from the same UDP socket and local port that received HELLO
both sides send periodic PING to keep mappings warm
```

Forbidden behavior:

```text
server receives on UDP port A
server replies from a new random UDP port B
```

That breaks common NAT and firewall conntrack behavior because the reverse
packet no longer matches the inbound mapping.

If a deployment performs only DNAT without the necessary SNAT or return-route
binding, the tunnel protocol cannot force the host OS to use the ingress path.
The protocol can only avoid making the situation worse by preserving the
received UDP socket and observed remote address.

## Error Handling Summary

- Non-HELLO control frames for an unknown session reply with session-scope
  CLOSE reason `unknown_session` when the transport leg is reply-capable.
- Non-HELLO frames for an unknown lane are dropped.
- Invalid body lengths are dropped.
- TCP invalid framing closes the TCP transport leg.
- UDP invalid frames are dropped without response.
- FEC recovery failure does not trigger retransmission.

## Open Items Before Implementation

- Decide whether SLC receive-window eviction also needs a time-based timeout in
  addition to the implementation's bounded memory limit.
- Decide whether TCP fallback legs are drained or closed immediately after UDP
  recovery.
- Tune the one-shot bandwidth probe ramp policy and UDP QoS threshold.
- Decide the packet-id exhaustion threshold that triggers graceful session
  rotation before `packet_id` wraps.
- Decide authentication/encryption separately. This draft only describes
  framing and transport behavior.
