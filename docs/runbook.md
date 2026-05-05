# Multipath Tunnel Runbook

This document describes the current runnable shape of the refactor.

## Build

```bash
go build ./...
```

Run with:

```bash
go run . -config /path/to/config.json
```

Opening and configuring a TUN device requires the privileges normally required
by the host OS for TUN and route changes.

## Server Example

```json
{
  "isServer": true,
  "server": {
    "listen": "0.0.0.0:9000"
  },
  "tun": {
    "name": "mp0",
    "localAddr": "10.0.0.1",
    "remoteAddr": "10.0.0.2",
    "allowedIPs": ["10.0.0.2/32"],
    "mtu": 1440
  },
  "fec": true,
  "fecFlushAlpha": 2,
  "fecFlushMinMs": 2,
  "fecFlushMaxMs": 30,
  "fecFlushColdStartMs": 20,
  "fecFlushFixedMs": 0,
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
```

The server listens on the same address for UDP and TCP. UDP receives normal
lane traffic. TCP is used by a lane only when that lane falls back.

## Client Example

```json
{
  "client": {
    "remotePaths": [
      {
        "remoteAddr": "203.0.113.10:9000",
        "weight": 1
      },
      {
        "remoteAddr": "198.51.100.20:9000",
        "weight": 1
      }
    ]
  },
  "tun": {
    "name": "mp0",
    "localAddr": "10.0.0.2",
    "remoteAddr": "10.0.0.1",
    "allowedIPs": ["10.0.0.1/32"],
    "mtu": 1440
  },
  "fec": true,
  "fecFlushAlpha": 2,
  "fecFlushMinMs": 2,
  "fecFlushMaxMs": 30,
  "fecFlushColdStartMs": 20,
  "fecFlushFixedMs": 0,
  "probeIntervalMS": 200,
  "probeTimeoutMS": 600
}
```

Each `remotePaths` entry becomes one lane. Lane IDs are assigned from `1` in
configuration order. A lane prefers UDP and may fall back to TCP independently
of other lanes.

## Legacy TCP Flag

```json
{
  "tcp": true,
  "client": {
    "remotePaths": [
      {
        "remoteAddr": "203.0.113.10:9000",
        "weight": 1
      }
    ]
  },
  "tun": {
    "name": "mp0",
    "localAddr": "10.0.0.2",
    "remoteAddr": "10.0.0.1",
    "allowedIPs": ["10.0.0.1/32"],
    "mtu": 1440
  }
}
```

The legacy `tcp` field is accepted so older config files keep parsing, but the
current protocol ignores it. Client lanes bootstrap over UDP and fall back to TCP
per lane when UDP becomes unhealthy.

## Current Defaults

```text
TUN MTU           1440
TUN name          empty, OS auto-selects
FEC               enabled
FEC flush alpha   2, SRTT multiplier
FEC flush min     2 ms
FEC flush max     30 ms
FEC cold start    20 ms RTT input
FEC fixed flush   0, disabled
probe interval   200 ms
probe timeout    600 ms
path weight      1
promListenAddr    127.0.0.1:0
```

When FEC negotiates the variable-span SLC profile, the sender arms a flush timer
for partial repair groups. The default interval is:

```text
flush_ms = clamp(max_session_srtt * fecFlushAlpha, fecFlushMinMs, fecFlushMaxMs)
```

Before a session has RTT samples, `fecFlushColdStartMs` is used as the RTT input
to that formula. `fecFlushFixedMs > 0` overrides the adaptive calculation and is
intended for deterministic latency tests.

Relevant metrics:

```text
multipath_lane_rtt_ms{session,lane,leg}
multipath_fec_events_total{event,session,source_span}
multipath_fec_flush_total{session,source_span}
```

Set `"fec": false` explicitly to disable FEC.

When `tun.name` is omitted, the OS chooses an available TUN device name and the
runtime configures that actual device name. When `promListenAddr` is omitted,
the default uses port `0` so the metrics listener binds an available local port
instead of failing on a fixed occupied port. Set `promListenAddr` to a concrete
address such as `"127.0.0.1:9100"` when Prometheus should scrape a stable port.
Set it to `"off"` to disable the listener.

The process prints the resolved metrics listen address at startup. Metrics are
served at `/metrics` in Prometheus text format. Current counters cover TUN
read/write, UDP/TCP transport read/write and errors, protocol frame tx/rx,
schedule strategy lane pick/skip/no-runnable events, lane/probe/fallback events,
and FEC repair/recovery events.

## Verification

Fast gates:

```bash
go build ./...
go test ./...
```

Real Linux E2E:

```bash
scripts/e2e.sh
```

The real E2E builds the current binary, creates two Linux network namespaces,
connects them with two veth paths, starts client/server with real TUN devices,
and runs protocol-level cases for multipath scheduling, per-lane fallback
isolation, concurrent multi-lane fallback, legacy `tcp` flag compatibility,
UDP-to-TCP fallback, fallback dial error (no runnable lane), bandwidth-probe
QoS leg selection, NAT traversal, FEC weak-net comparison, FEC over TCP
fallback, multipath plus FEC, FEC loaded latency under iperf3 UDP background
traffic, weighted scheduling, and near-MTU packet survival. It requires Linux, `go`, root
privileges, `ip`, `tc`, `ping`, and `iptables`. Run it as a regular user when
possible; the script builds the binary before escalating for network namespace
setup. `iperf3` and `timeout` enable an
optional throughput smoke.

The NAT case adds a router namespace between the client and server namespaces,
enables IPv4 forwarding, and SNATs the client-side underlay subnet toward the
server. TCP fallback is blocked in that case, so a passing TUN ping proves the
UDP leg works through NAT/conntrack.

The FEC comparison runs the same one-lane scenario with `fec=false` and
`fec=true` under 20% client-to-server UDP tunnel loss while TCP fallback is
blocked. The script prints both observed ping packet-loss values and requires the
FEC case to be lower. See `docs/e2e-evaluation.md` for interpretation and
limits. By default each FEC sample sends 1000 ping packets at 20ms intervals;
override `MULTIPATH_REAL_E2E_FEC_PING_COUNT` or
`MULTIPATH_REAL_E2E_FEC_PING_INTERVAL` for faster local smoke runs. The script
also repeats the FEC comparison under added UDP tunnel delay; the default is
`50ms` one-way and can be changed with
`MULTIPATH_REAL_E2E_FEC_HIGH_RTT_DELAY`.

The script starts client and server with `MULTIPATH_DEBUG=1` by default, so each
case writes verbose protocol, transport, probe, fallback, and FEC traces to
per-side `${case}.client.log` and `${case}.server.log` files in the work
directory. Set `MULTIPATH_REAL_E2E_DEBUG=0` to keep those logs quiet for routine
runs.

The same script can be invoked through Go's test runner when explicitly
enabled:

```bash
MULTIPATH_REAL_E2E=1 go test -run TestRealE2EScript .
```

Protocol encode benchmark:

```bash
go test -bench=BenchmarkEncode -benchmem ./internal/protocol
```

## Operational Notes

- IPv4 underlay is the current design target.
- The recommended TUN MTU is `1440` when UDP/TCP fallback and 4+1 FEC are both
  enabled.
- UDP replies for a lane use the same UDP socket that received the packet and
  the observed remote address.
- Transport writers consume packet buffers synchronously. A future asynchronous
  transport must copy payload bytes at the transport boundary before retaining
  them.
