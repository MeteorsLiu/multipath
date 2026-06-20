# Real E2E Evaluation

This document describes what the real Linux E2E script is intended to prove
and how to interpret its results.

## Test Matrix

| Test | Topology | Fault | Main Signal |
| --- | --- | --- | --- |
| Multipath | two UDP-first lanes over two veth paths | path1 100% loss, path2 100% loss, both paths 100% loss, recovery | schedule strategy keeps traffic alive while at least one lane works and fails closed when no lane works |
| Per-lane fallback | two UDP-first lanes over two veth paths | only path2 UDP tunnel traffic is dropped (TCP and path1 stay clean) | lane=2 falls back to TCP while lane=1 keeps using UDP, and lane=2 returns to UDP after the block clears |
| Concurrent multi-lane fallback | two UDP-first lanes over two veth paths | UDP tunnel traffic is dropped on path1 and path2 simultaneously | both lanes fall back to TCP independently, ping survives, and both lanes return to UDP after the block clears |
| Legacy TCP flag | two UDP-first lanes with legacy `tcp: true` config | client TCP dials to the server port are dropped, then path2 is dropped | old configs still parse, but `tcp: true` no longer forces TCP-only bootstrap |
| Fallback | one UDP-first lane over one veth path | UDP tunnel traffic is dropped while TCP is clean, then TCP traffic is dropped after UDP is restored | a lane falls back to TCP when UDP fails and recovers back to UDP |
| Unknown-session rebootstrap | one UDP-first lane over one veth path | the server process is restarted while the client keeps probing the stale session | the restarted server replies `CLOSE{unknown_session}`, the client rebuilds a fresh session, and ping recovers |
| Multi-lane rebootstrap | two UDP-first lanes over two veth paths | the server process is restarted while the client keeps probing the stale session | the restarted server rejects the stale session, the client rebuilds once, and both configured lanes accept fresh UDP HELLO_ACKs |
| Fallback dial error | one UDP-first lane over one veth path | UDP tunnel traffic is dropped and the server REJECTs incoming TCP with TCP RST | the client emits `send/dialer: dial err` for the lane and ping fails closed because no transport leg is runnable |
| TCP established redial | one UDP-first lane over one veth path | UDP is blocked, TCP fallback is established, then the server process is killed and restarted | the client observes an established TCP leg failure, redials TCP, accepts a new TCP HELLO_ACK, and ping recovers while UDP remains blocked |
| Leg selector | one UDP-first lane with warm TCP fallback over one veth path | 50% large-packet UDP loss degrades the initial bandwidth-probe ramp while TCP stays clean | bandwidth probe samples are recorded and both client and server leg selectors send DATA frames over TCP even though UDP remains active |
| Bandwidth probe TCP reference | one UDP-first lane with warm TCP fallback over one veth path | UDP tunnel traffic is rate-limited while TCP stays clean | the client records a TCP reference sample before its UDP sample and the UDP sample falls in the expected rate window |
| Bandwidth probe default cap | one UDP-first lane with warm TCP fallback over one veth path | 50% large-packet UDP loss while the default cap is used as reference | the client records a capped UDP sample and then sends DATA frames over TCP |
| Bandwidth probe disabled | one UDP-first lane over one veth path | `MULTIPATH_DISABLE_BW_PROBE=1` | ping traffic works while neither side emits `send/bw` logs |
| NAT | client namespace behind a router namespace doing SNAT to the server namespace | TCP fallback is blocked | UDP HELLO/ACK, DATA, and probes work through NAT/conntrack using observed source addresses |
| NAT + TCP fallback | client namespace behind a router namespace doing SNAT to the server namespace | UDP tunnel traffic is dropped through NAT while TCP stays clean | the same lane establishes TCP fallback through SNAT and ping recovers |
| FEC weak-net comparison | one UDP-first lane over one veth path | 20% client-to-server UDP tunnel loss with TCP fallback blocked | `fec=true` reduces observed tunnel packet loss versus `fec=false` |
| FEC high-RTT weak-net comparison | one UDP-first lane over one veth path | 20% client-to-server UDP tunnel loss, added UDP tunnel delay in both directions, TCP fallback blocked | FEC loss reduction still holds while ping RTT shows recovery-delay impact |
| FEC disabled negotiation | one UDP-first lane with `fec=false` | clean network with short DATA load | HELLO/HELLO_ACK do not negotiate FEC or LINK_STATUS, and neither REPAIR nor LINK_STATUS is emitted |
| FEC over TCP fallback | one UDP-first lane with `fec=true` | UDP tunnel traffic is dropped while TCP is clean | the lane falls back to TCP and the TCP HELLO_ACK preserves the FEC capability and `fec_profile` |
| Multipath + FEC | two UDP-first lanes with `fec=true` | path1 has 20% UDP tunnel loss and TCP fallback blocked; path2 stays clean | observed ping packet loss stays under 10%, validating combined multipath spreading and FEC recovery |
| LINK_STATUS QoS | one UDP-first lane with `fec=true` and warm TCP shadow | REPAIR is observed on TCP shadow, then 20% client-to-server UDP tunnel loss is applied and later cleared while bandwidth probe is disabled | server receive-side FEC differential QoS emits a UDP-limited `LINK_STATUS` snapshot on change, client applies it, client DATA switches to TCP, then returns to UDP after a clear snapshot |
| LINK_STATUS reverse QoS | one UDP-first lane with `fec=true` and warm TCP shadow | server-to-client DATA sees 20% UDP tunnel loss and later clears while bandwidth probe is disabled | client receive-side FEC differential QoS emits a UDP-limited `LINK_STATUS` snapshot on change, server applies it, server DATA switches to TCP, then returns to UDP after a clear snapshot |
| LINK_STATUS iperf QoS | one UDP-first lane with `fec=true`, warm TCP shadow, bandwidth probe disabled, and a staged TCP iperf3 flow over the TUN | UDP tunnel traffic is rate-limited after a baseline window and later restored | interval iperf3 windows show throughput remains usable under UDP QoS and recovers after clear; LINK_STATUS limited/clear snapshots and selector switch counts stay bounded |
| LINK_STATUS repeated iperf QoS | same as LINK_STATUS iperf QoS | UDP tunnel rate limit is applied, cleared, applied again, and cleared again during one iperf3 flow | both QoS cycles produce limited/clear snapshots, throughput recovers after each clear, and selector switching does not flap beyond the expected cycles |
| LINK_STATUS jitter iperf QoS | same as LINK_STATUS iperf QoS with added tunnel RTT/jitter on UDP and TCP | UDP tunnel traffic has RTT/jitter plus rate limit, then returns to RTT/jitter without rate limit | QoS detection and recovery still work when the underlay has high delay variance |
| LINK_STATUS RTT200 jitter iperf QoS | same as LINK_STATUS jitter iperf QoS | both directions carry about 100ms base delay plus high jitter; UDP rate limit is applied and cleared | QoS detection and recovery still work at roughly 200ms base RTT with high jitter |
| TCP fallback rate dynamics | one UDP-first lane with `fec=true`, warm TCP shadow, bandwidth probe disabled, and UDP tunnel traffic blocked | DATA runs over TCP fallback; TCP tunnel traffic is rate-limited and then restored while UDP remains blocked | iperf3 interval windows show TCP shaping is actually hit and throughput recovers after TCP rate limit clears |
| FEC loaded latency | one UDP-first lane with `fec=true` | 20% client-to-server UDP tunnel loss, TCP fallback blocked, iperf3 UDP background load fills the FEC group quickly | sparse ping reports the realistic loaded-latency RTT distribution (line `rtt min/avg/max/mdev = ...`) so that FEC recovery delay can be evaluated under traffic instead of under sparse ping |
| Weighted scheduling | two UDP-first lanes with `weight: 4` and `weight: 1` | clean network | observed client-side DATA `schedule_select` lane=1 fraction tracks `4/(4+1)` within ±0.15, validating per-lane weight handling |
| MTU | one UDP-first lane over one veth path | clean network, then UDP tunnel traffic dropped to force TCP fallback | near-MTU pings (`ping -s 1412 -M do`) survive both UDP transport and TCP fallback, validating tunnel header overhead math |
| MTU + FEC | one UDP-first lane with `fec=true` | near-MTU pings on UDP and then TCP fallback | near-MTU DATA and REPAIR frames are accepted without FEC recovery errors |

The FEC comparison intentionally uses one lane. Multipath failover would hide
some losses and make it harder to isolate the FEC signal. TCP fallback is also
blocked during the weak-net sample so the comparison measures FEC rather than
transport fallback.

## NAT Method

The NAT case adds a router namespace between the client and server namespaces:

```text
client namespace -> NAT namespace -> server namespace
```

The NAT namespace enables IPv4 forwarding and SNATs the client-side underlay
subnet to the NAT namespace's server-facing address. The client dials the
server-facing underlay address, and TCP fallback is blocked on the client side.
Passing ping over the TUN proves that UDP session setup and data forwarding work
through conntrack, including server replies to the observed UDP source address.

## FEC Comparison Method

The script runs two cases with the same namespace topology and the same ping
workload:

```text
case A: fec=false
case B: fec=true
```

For each case it starts a real server and client, waits for baseline ping over
the TUN to succeed, then applies filtered `tc` loss to client-to-server UDP
tunnel traffic:

```bash
tc qdisc replace dev <client-path1> root handle 1: prio bands 4
tc qdisc replace dev <client-path1> parent 1:3 handle 30: netem loss 20%
tc filter replace dev <client-path1> protocol ip parent 1:0 prio 1 u32 \
  match ip protocol 17 0xff \
  match ip dport <server-port> 0xffff \
  flowid 1:3
```

The loss is applied only on the client-to-server UDP tunnel direction. That
makes lost tunnel DATA packets recoverable when the corresponding REPAIR frame
arrives. The return path is left clean so the measured ping loss mainly reflects
whether client-to-server tunnel packets survive. TCP dials to the same server
port are dropped during this sample so fallback cannot hide UDP loss.

The script records Linux `ping` packet loss for both cases. Each sample sends
1000 packets at 20ms intervals to reduce random `tc netem` variance. The pass
condition requires:

```text
fec_on_loss < fec_off_loss
```

After the normal FEC comparison, the script repeats the same `fec=false` and
`fec=true` cases with 50ms added UDP tunnel delay in both directions. The
high-RTT case is intended to expose how FEC recovery changes ping RTT and max
latency, not just packet-loss rate. Because the `fec=false` and `fec=true`
high-RTT samples use independent random loss streams, the high-RTT case prints
the packet-loss comparison but gates on FEC actually emitting recovered packets
without recovery errors.

## FEC Loaded Latency Method

Sparse ping (e.g., one packet every 20ms) was a worst case for the fixed 4+1 SLC
profile because each repair group needed four DATA frames before the REPAIR was
emitted. The variable-span SLC profile bounds this added recovery delay with the
sender's FEC flush timer. Under the fixed test knob, expected recovered-packet
RTT is bounded by:

```text
baseline_rtt + fecFlushFixedMs + scheduling/jitter slack
```

With adaptive flushing, replace `fecFlushFixedMs` with:

```text
clamp(max_session_srtt * fecFlushAlpha, fecFlushMinMs, fecFlushMaxMs)
```

The loaded-latency case loads the tunnel with a 10Mbit/s iperf3 UDP background
flow for 25 seconds, then runs 400 sparse ping probes at 50ms intervals
concurrently to measure the RTT distribution that an interactive flow would
observe. The sparse ping is the measurement; the iperf3 stream verifies the
loaded path while the flush timer keeps partial FEC groups from stalling under
sparse traffic.

The ping output is recorded under `${case}.ping.log` and the iperf3 logs under
`${case}.iperf-client.log` and `${case}.iperf-server.log`. The pass condition is
that ping produces an `rtt min/avg/max/mdev` summary line; the actual RTT
numbers are reported but not gated.

The case is skipped if `iperf3` is not installed.

## Expected Result

With `fec=false`, observed tunnel loss should roughly track the injected
client-to-server UDP tunnel loss.

With `fec=true`, observed tunnel loss should be lower because the current 4+1
SLC profile sends one REPAIR frame for every four DATA frames. A single missing
DATA frame in a repair group can be reconstructed if the other DATA frames and
the REPAIR frame arrive.

The expected result is not zero loss. The current profile cannot recover:

- two or more lost DATA frames in the same four-packet repair group
- a lost DATA frame when the corresponding REPAIR frame is also lost
- burst loss that spans multiple symbols in the same repair group

## Overhead And Tradeoff

The current profile sends one REPAIR for four DATA frames, so the nominal FEC
bandwidth overhead is about 25% for protected DATA traffic before outer IP/UDP
headers.

Recovery is delayed until the receiver has enough symbols to reconstruct the
missing packet. This is useful for hiding isolated loss from upper layers, but
it is not a retransmission protocol and does not guarantee delivery.

## Limits Of This Evaluation

This is a functional weak-network smoke test, not a rigorous benchmark.
`tc netem` random loss and `ping` sampling can vary between runs. If the FEC
case does not beat the non-FEC case, inspect the ping logs in the script's
printed work directory plus the per-side `${case}.client.log` and
`${case}.server.log` protocol traces, then rerun before drawing a performance
conclusion.

For a real benchmark, run multiple samples per case and record:

- packet loss percentage
- RTT distribution
- throughput with `iperf3`
- CPU usage
- tunnel bytes sent per delivered packet
