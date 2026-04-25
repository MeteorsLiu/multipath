# Real E2E Evaluation

This document describes what the real Linux E2E script is intended to prove
and how to interpret its results.

## Test Matrix

| Test | Topology | Fault | Main Signal |
| --- | --- | --- | --- |
| UDP multipath | two UDP lanes over two veth paths | path2 100% loss, both paths 100% loss, recovery | scheduler keeps traffic alive while at least one lane works |
| TCP bootstrap | two TCP lanes over two veth paths | path2 100% loss, both paths 100% loss, recovery | TCP leg can carry a lane and still follows multipath scheduling |
| FEC weak-net comparison | one UDP lane over one veth path | 20% client-to-server `tc netem` loss | `fec=true` reduces observed tunnel packet loss versus `fec=false` |

The FEC comparison intentionally uses one lane. Multipath failover would hide
some losses and make it harder to isolate the FEC signal.

## FEC Comparison Method

The script runs two cases with the same namespace topology and the same ping
workload:

```text
case A: fec=false
case B: fec=true
```

For each case it starts a real server and client, waits for baseline ping over
the TUN to succeed, then applies:

```bash
tc qdisc replace dev <client-path1> root netem loss 20%
```

The loss is applied only on the client-to-server underlay direction. That makes
lost tunnel DATA packets recoverable when the corresponding REPAIR frame
arrives. The return path is left clean so the measured ping loss mainly reflects
whether client-to-server tunnel packets survive.

The script records Linux `ping` packet loss for both cases and requires:

```text
fec_on_loss < fec_off_loss
```

## Expected Result

With `fec=false`, observed tunnel loss should roughly track the injected
client-to-server underlay loss.

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
printed work directory and rerun before drawing a performance conclusion.

For a real benchmark, run multiple samples per case and record:

- packet loss percentage
- RTT distribution
- throughput with `iperf3`
- CPU usage
- tunnel bytes sent per delivered packet
