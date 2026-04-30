# Leg Selector Design

Date: 2026-04-29

## Overview

Introduce a two-level scheduler that picks both a lane and, within that lane,
the appropriate transport leg (UDP or TCP) based on measured connection quality.

The lane-level scheduler (`schedule.Strategy`) is unchanged. The new leg-level
selector lives in the `send` package and is informed by per-leg quality metrics.

## Motivation

ISPs may QoS UDP traffic (rate-limit / police excess packets) without affecting
TCP on the same path. RTT does not detect this — token-bucket policers drop
excess packets at normal latency. The sender must measure delivery rate
(end-to-end frame delivery within a deadline) to distinguish healthy from
degraded legs.

## Architecture

### Module Boundaries

No new public package. All additions are within `internal/tunnel/send/`:

- `LegSelector` interface — pluggable leg-picking policy
- `legQualityTracker` — per-leg delivery-rate measurement
- Default implementations: `QualityLegSelector`, `UDPPreferssSelector`

`schedule.Strategy` and `transport` are unaffected.

### Data Flow

```
Send.Write(packet)
  → handleTUNPacket(packet)
    → writeTUNPacket(sessionID, packet)
      → writeScheduledFrame(frame)
        → l.pickLane(sessionID, cost)                   ← schedule.Strategy (unchanged)
        → udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()  ← value-type snapshots
        → useUDP, ok := l.legSelector(sessionID).Pick(udpQ, tcpQ)  ← Send-owned
        → leg = udpLeg if useUDP else tcpLeg            ← caller maps result to leg
        → l.enqueueFrameWithSize(ctx, leg, frame)
```

### Interfaces

```go
// LegQuality describes measured quality of one transport leg.
type LegQuality struct {
    Active       bool
    DeliveryRate float64       // 0.0–1.0, fraction of probes delivered within deadline
    SmoothedRTT  time.Duration
    RTTVariance  time.Duration
}

// LegSelector selects which leg within a lane to use.
// Returns useUDP=true for UDP, useUDP=false for TCP, and ok=false
// when neither leg is usable.
type LegSelector interface {
    Pick(udp, tcp LegQuality) (useUDP bool, ok bool)
}
```

### Quality Measurement

Each leg maintains a `legQualityTracker` in `laneRuntime`:

```go
type legQualityTracker struct {
    mu            sync.Mutex
    onTimeCount   uint32    // probes replied within deadline
    totalCount    uint32    // total probes in current window
    windowSentAt  uint64    // ms timestamp when window started
}
```

#### Probe flow

Existing PING/PONG per leg (UDP and TCP) already provides RTT samples. The
change adds a **deadline** to each pending PING:

```
deadline_ms = last_SRTT * 2, floor(100ms)
```

PONG arrival:

- If `now_ms <= deadline_ms`: mark on-time delivery, increment `onTimeCount`
- Always: increment `totalCount`

Window reset (every `probeInterval * N` or `> minSamples` probes):

```
deliveryRate = float64(onTimeCount) / float64(totalCount)
```

#### What delivery rate means

| | UDP | TCP |
|---|---|---|
| PING loss | packet dropped → counted as missed | TCP retransmits, may arrive before deadline |
| PING delayed | arrives after deadline → counted as missed | TCP retransmits may cause delay beyond deadline |
| Bottom line | DeliveryRate is comparable across both legs |

### Default Selector: QualityLegSelector

```go
type QualityLegSelector struct{}

func (QualityLegSelector) Pick(udp, tcp LegQuality) (useUDP bool, ok bool) {
    switch {
    case !udp.Active && !tcp.Active:
        return false, false
    case udp.Active && !tcp.Active:
        return true, true
    case !udp.Active && tcp.Active:
        return false, true
    }

    // Both active. Prefer UDP unless it is clearly degraded.
    const minUDPDelivery = 0.80
    const minTCPDelivery = 0.90

    // Delivery rate below threshold: token-bucket policer (drops excess).
    if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
        return false, true
    }

    // RTT variance exceeds mean: shaper (bufferbloat, jitter).
    if udp.RTTVariance > 0 && udp.RTTVariance >= udp.SmoothedRTT &&
        tcp.DeliveryRate >= minTCPDelivery {
        return false, true
    }

    return true, true
}
```

### Backup Selector: UDPPreferssSelector

Equivalent to current hardcoded `selectLegLocked`:

```go
type UDPPreferssSelector struct{}

func (UDPPreferssSelector) Pick(udp, tcp LegQuality) (useUDP bool, ok bool) {
    switch {
    case udp.Active:
        return true, true
    case tcp.Active:
        return false, true
    default:
        return false, false
    }
}
```

### laneRuntime Changes

- Remove `selectLegLocked` (replaced by `LegSelector`)
- Add `legQualities() (udpLeg transport.LegRef, udpQ LegQuality, tcpLeg transport.LegRef, tcpQ LegQuality)`
- Add per-leg `legQualityTracker` fields

### Send Changes

`Send` holds a per-session `LegSelector` map (like `strategies`):

```go
type Send struct {
    // ...existing fields...
    legSelectors map[uint64]LegSelector
}
```

`legSelector(sessionID)` follows the same lazy-init pattern as `strategy()`:

```go
func (l *Send) legSelector(sessionID uint64) LegSelector {
    l.sessionStatesMu.RLock()
    sel := l.legSelectors[sessionID]
    l.sessionStatesMu.RUnlock()
    if sel != nil {
        return sel
    }
    l.sessionStatesMu.Lock()
    defer l.sessionStatesMu.Unlock()
    if sel := l.legSelectors[sessionID]; sel != nil {
        return sel
    }
    sel = &QualityLegSelector{}
    l.legSelectors[sessionID] = sel
    return sel
}
```

### ProbeLoop Integration

No structural change. Existing PING/PONG loop already covers all active legs.
The change is:

1. Store deadline with each pending PING in `rttPending`
2. On PONG, call `laneRuntime.recordDelivery(pingKey, arrivedBeforeDeadline)`
3. LegQuality is computed on demand by `legQualities()`, not by a timer

## Trade-offs

- **QualityLegSelector default**: Simple threshold-based, avoids flapping via
  delivery-rate window. Tunable thresholds if needed.
- **UDPPreferssSelector backup**: Zero behavioral change for deployments that
  don't need quality-aware leg selection.
- **Lane still picks leg**: Decision stays in `Send`; `laneRuntime` only
  provides data. LegSelector is per-session, pluggable, testable independently.

## Open Questions (Post-MVP)

- Burst probing: send N back-to-back PINGs to detect policer token-bucket depth.
  Not needed for initial delivery-rate-based approach but may improve detection
  speed.
- Configurable thresholds per deployment.
