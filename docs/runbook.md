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
  "probeIntervalMS": 1000,
  "probeTimeoutMS": 3000
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
  "probeIntervalMS": 1000,
  "probeTimeoutMS": 3000
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
probe interval   1000 ms
probe timeout    3000 ms
path weight      1
promListenAddr    127.0.0.1:0
```

Set `"fec": false` explicitly to disable FEC.

When `tun.name` is omitted, the OS chooses an available TUN device name and the
runtime configures that actual device name. When `promListenAddr` is omitted,
the default uses port `0` so a future metrics listener can bind an available
port instead of failing on a fixed occupied port.

`promListenAddr` is accepted for compatibility with old config files. The
current refactor does not start a Prometheus metrics server yet.

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
checks UDP bootstrap and legacy `tcp` flag compatibility, injects path loss with
`tc netem`, verifies ping recovery over the tunnel, and runs a weak-network FEC
comparison. It requires Linux, `go`, root privileges, `ip`, `tc`, and `ping`.
Run it as a
regular user when possible; the script builds the binary before escalating for
network namespace setup. `iperf3` and `timeout` enable an optional throughput
smoke.

The FEC comparison runs the same one-lane UDP scenario with `fec=false` and
`fec=true` under 20% client-to-server underlay loss. The script prints both
observed ping packet-loss values and requires the FEC case to be lower. See
`docs/e2e-evaluation.md` for interpretation and limits.

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
