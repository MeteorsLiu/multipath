# Multipath Tunnel Protocol Draft

This document describes the proposed v2 wire protocol for the multipath
tunnel refactor.

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

With FEC enabled, the receiver keeps bounded SLC window bookkeeping keyed by
`packet_id` and `base_packet_id`. That bookkeeping exists only to recover lost
tunnel packets and avoid emitting a recovered packet twice if the original
arrives late. It is not a general reliable-transport deduplication layer.

## Terminology

- Session: one logical tunnel between a client and a server.
- Lane: one independently scheduled logical path inside a session.
- Transport leg: the concrete carrier for a lane, currently UDP or TCP.
- Fallback: a per-lane decision to use TCP while the same lane's UDP leg is
  unavailable.
- Frame: one protocol message after the UDP or TCP transport envelope.
- SLC: the protocol's sliding linear coding layer. SLC profiles use random
  linear coding over GF(2^8) with DATA shards and one REPAIR shard.
- DATA symbol: the FEC-protected representation of one tunnel IP packet.
- REPAIR symbol: one FEC parity symbol generated from DATA symbols.
- packet_id: a session-scoped DATA identifier used by the SLC window.
- base_packet_id: the first DATA identifier protected by one REPAIR frame.
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
```

`session_id` identifies the tunnel. `lane_id` identifies the lane that carries
this frame and is used for scheduling, health tracking, and per-lane fallback.
FEC scope is not derived from `lane_id`; SLC uses `packet_id` and
`base_packet_id`.

Unknown versions or frame types are dropped. On TCP, repeated invalid frames
should close the transport leg.

## Capability Bits

`caps` is a `uint16` bitset used in HELLO and HELLO_ACK:

```text
bit 0 = TCP fallback supported
bit 1 = FEC supported
```

`fec_profile` is a `uint8`:

```text
0 = off
1 = slc_4_plus_1
2 = slc_variable_plus_1
```

Both peers use the capability intersection returned in HELLO_ACK. If both peers
support FEC, the negotiated `fec_profile` is the highest profile supported by
both peers.

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
4. Retry HELLO until HELLO_ACK arrives or the attempt times out.

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

PING probes liveness and keeps NAT mappings warm.

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
5. Send DATA on that lane's UDP leg if UDP is active.
6. Otherwise send DATA on that lane's TCP leg if TCP fallback is active.
7. Do not select a lane with no usable transport leg.
8. If the DATA frame is written successfully and FEC is enabled, insert
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
base_packet_id uint32
key            uint16
source_span    uint8
repair_symbol bytes[repair_symbol_size]
```

`source_span` is the number of contiguous DATA symbols protected by this REPAIR
frame. It defines the protected range:

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
repair count    = 1
field           = GF(2^8)
```

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
`source_span`:

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

1. After a DATA frame is written successfully, add its DATA symbol to the SLC
   transmit window.
2. After four contiguous successfully written DATA symbols, compute one
   REPAIR.
3. Set `base_packet_id` to the first protected DATA symbol.
4. Increment or otherwise vary `key` for each REPAIR.
5. Set `source_span = 4`.
6. Set the repair symbol length to the largest IP packet length in this repair
   window.
7. Compute the REPAIR symbol with coefficients derived from `key`.
   Source bytes beyond the end of a shorter IP packet are treated as zero.
8. Send REPAIR on any healthy lane.

If fewer than four contiguous DATA symbols are available, `slc_4_plus_1` does
not emit REPAIR.

Sender behavior for `slc_variable_plus_1`:

1. After a DATA frame is written successfully, add its DATA symbol to the SLC
   transmit window.
2. Arm a flush timer when the pending SLC transmit window transitions from zero
   symbols to one symbol.
3. If four contiguous successfully written DATA symbols become available before
   the timer fires, cancel the timer and emit one full REPAIR with
   `source_span = 4`.
4. If the timer fires with one to three contiguous DATA symbols pending, emit
   one partial REPAIR with `source_span` set to the number of protected symbols.
5. Set `base_packet_id` to the first protected DATA symbol.
6. Increment or otherwise vary `key` for each REPAIR.
7. Set the repair symbol length to the largest IP packet length in this repair
   window.
8. Compute the REPAIR symbol with coefficients derived from `key`.
   Source bytes beyond the end of a shorter IP packet are treated as zero.
9. Send REPAIR on any healthy lane.

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
2. Validate `source_span` for the negotiated `fec_profile`.
3. Use `base_packet_id` and `source_span` to identify the protected DATA
   symbols.
4. Store the REPAIR symbol while the protected window is still alive.
5. If all protected DATA symbols are already known, drop the REPAIR.
6. If exactly one protected DATA symbol is missing, recover it using the REPAIR
   symbol and coefficients derived from `key`.
7. If multiple protected DATA symbols are missing, keep the REPAIR until more
   DATA arrives or the window expires. A single SLC REPAIR cannot recover two
   missing DATA symbols.
8. For each recovered DATA symbol, parse the IP header to obtain the packet
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

## Lane State Machine

Each lane owns independent UDP and TCP transport-leg health.

```text
new
  -> udp_probing
  -> udp_active
  -> udp_suspect
  -> tcp_connecting
  -> tcp_active
  -> closed
```

Important transitions:

```text
udp_probing + HELLO_ACK over UDP -> udp_active
udp_active + PING timeout        -> udp_suspect
udp_suspect + TCP HELLO_ACK      -> tcp_active
tcp_active + UDP PONG            -> udp_active
open + CLOSE                     -> closed
```

Fallback is per lane. If lane A falls back to TCP, lane B can continue on UDP.

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
TUN packet -> DATA packet_id=101 -> lane 2 UDP
TUN packet -> DATA packet_id=102 -> lane 1 UDP
TUN packet -> DATA packet_id=103 -> lane 2 UDP
             REPAIR base_packet_id=100 key=7 source_span=4 -> any healthy lane
```

`lane_id` on REPAIR describes the lane that carries the REPAIR frame. It does
not restrict which DATA symbols the REPAIR can protect.

## UDP to TCP Fallback Flow

Only the failed lane falls back.

```text
lane 2 UDP PING timeout
Client opens lane 2 TCP leg
Client -> Server over TCP: HELLO(session, lane=2)
Server -> Client over TCP: HELLO_ACK(session, lane=2)

lane 1 continues over UDP
lane 2 DATA and REPAIR use TCP while UDP is unhealthy
```

The client continues low-rate UDP PING for lane 2. If UDP recovers:

```text
Client -> Server over UDP: PING(session, lane=2)
Server -> Client over UDP: PONG(session, lane=2)
lane 2 switches back to UDP
TCP leg is closed immediately or after a short drain period
```

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
- Decide the packet-id exhaustion threshold that triggers graceful session
  rotation before `packet_id` wraps.
- Decide authentication/encryption separately. This draft only describes
  framing and transport behavior.
