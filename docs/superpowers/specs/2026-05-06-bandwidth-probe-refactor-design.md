# Bandwidth Probe Refactor

Date: 2026-05-06

## Overview

Rewrite `internal/tunnel/send/bandwidth_probe.go` to enforce the design
intent from `docs/protocol.md` and `docs/architecture.md`:

- TCP probe establishes a reference bandwidth.
- UDP probe starts only after the TCP reference exists.
- UDP rate is capped at the TCP reference; never exceeds it.
- UDP probing answers one question: can UDP approach the TCP reference
  without material loss? If not, QoS is detected.

The current code violates this: `advanceBandwidthProbeRate` does not cap
UDP rate against TCP reference, and the stop conditions rely on ACK-based
`stepBps` measurements that can be zero under delay, causing runaway rate
growth.

## Motivation

Per design doc rules:

- "Increase UDP probe rate gradually **toward** the TCP reference"
  (`protocol.md:760`)
- "Send must not keep increasing UDP probe traffic merely to discover
  UDP's absolute ceiling after the relative TCP-vs-UDP decision is clear"
  (`architecture.md:240`)

The refactored module must guarantee these invariants structurally, not
through reactive stop conditions.

## Architecture

### Module Boundaries

All changes stay within `internal/tunnel/send/bandwidth_probe.go` and
`bandwidth_probe_test.go`. External call signatures are preserved:

- `probeBandwidth(ctx, now)` — entry from ProbeLoop
- `receiveBandwidthProbe(ctx, sessionID, laneID, leg, body) error` — from RecvState
- `receiveBandwidthProbeAck(sessionID, laneID, leg, body) error` — from RecvState
- `clearBandwidthLeg(leg)` — lane teardown

Internal functions, state types, constants, and stop-condition helpers may
be fully redesigned.

### Two-Phase Probe Flow

```
session established
  ↓
TCP probe (if TCP leg exists and needs probing):
  start at 16 Mbps → advance +10Mbps/step → stop on plateau
  ↓
record refBps = maxStepBps in lane quality
  ↓
UDP probe (if UDP leg exists and needs probing):
  start at 16 Mbps → add +10Mbps/step → stop on loss ≥ 1%
  ↓
  stop on: loss ≥ 1%, OR stepBps plateau while rate < cap
  ↓
record result in lane quality (QoS-limited or not)
```

### Stop Decision Matrix

| Condition | TCP | UDP |
|-----------|-----|-----|
| stepBps plateau (2 steps, growth < 5%) | **Stop** | N/A (plateau does not stop UDP) |
| loss ≥ 1% | N/A | **Stop**, QoS detected |
| 10 s window expired | N/A | **Stop**, no QoS |

TCP has no hard window bound; plateau stops it immediately. A safety
deadline of 30 s prevents infinite probes.

UDP has a 10 s hard window. Only loss ≥ 1% stops it early; otherwise
it runs the full window.

## Data Flow

### Probe train (runProbeTrain)

```
probeTrain:
  deadline = now + (leg==TCP ? 30s : 10s)
  for now < deadline:
    step = runStep(rate)        // 500 ms window, send frames, collect ACKs
    measure: stepBps, stepLoss

    if leg == TCP && plateau(stepBps):
      break

    if leg == UDP:
      if stepLoss >= 1%:          break (QoS)
      if plateau(stepBps) && rate < cap:  break (QoS)

    rate = nextRate(rate, cap, plateau)
    if rate == prev:              break
```

### RunStep

```
runStep(rate, leg):
  for now < stepDeadline (500 ms):
    round = newRound(rate, leg)
    send frames via rate.Limiter at rate bps
    record sent bytes
    // ACKs arrive asynchronously via receiveBandwidthProbeAck
    // building acked bytes on the step
  stepBps = ackedBytes * 8 / stepDuration
  stepLoss = (sentFrames - ackedFrames) / sentFrames
  return stepBps, stepLoss
```

### Rate Advance (nextRate)

```go
func nextRate(rate, cap uint64, growthStalled bool) uint64 {
    if cap > 0 && rate >= cap {
        return cap       // UDP: already at cap
    }
    if cap == 0 {
        // TCP: additive +10Mbps until plateau
        if growthStalled {
            return rate
        }
        return rate + 10_000_000
    }
    // UDP: adaptive approach to cap
    gap := cap - rate
    if gap > rate {
        return rate * 2
    }
    if gap > rate/4 {
        return rate + max(gap/2, 10_000_000)
    }
    return min(rate+5_000_000, cap)
}
```

### Plateau Detection

```
plateau(samples []uint64):
  if len(samples) < 3: return false
  // Growth < 5% over last 2 steps
  last2max = max(samples[N-1], samples[N-2])
  prev = samples[N-3]
  return last2max <= prev * 1.05
```

## State Structure

```go
type bandwidthProbeState struct {
    key         laneKey
    leg         transport.LegRef
    legKey      pingKey

    capBps      uint64          // 0 for TCP, refBps for UDP
    rateBps     uint64          // current target rate
    startedAt   time.Time
    endedAt     time.Time

    sentFrames  uint64
    ackedFrames uint64
    ackedBytes  uint64

    steps       map[uint64]*bandwidthProbeStep
    stepOrder   []uint64       // ordered list for plateau check

    lastLoss    float64
    inFlight    bool
    complete    bool
}
```

Removed fields: `rampChunks`, `prevStepBytes`, `prevStepLoss`,
`lastStepLoss`, `lastStepID`, `currentStepID`, `ewmaBps`,
`sampleCount`, `maxStepBps`, `stepStartedAt`, `lastStepBps`,
`lastStepBytes`, `lastRoundLoss`.

## Constants Kept

| Constant | Value | Purpose |
|----------|-------|---------|
| `bandwidthProbeWindow` | 10 s | UDP hard window |
| `bandwidthProbeRoundWindow` | 500 ms | per-step duration |
| `bandwidthProbeBurstWindow` | 2 ms | limiter burst |
| `bandwidthProbeMinRateBps` | 16 Mbps | absolute floor |
| `bandwidthProbeAdditiveStepBps` | 10 Mbps | used in adaptive gap |
| `bandwidthProbeMaxFrames` | 64 | per-round frame cap |
| `bandwidthProbeTCPPayloadSize` | 32 KB | TCP frame payload |
| `bandwidthProbeUDPMinPayloadSize` | 1200 | UDP min payload |
| `bandwidthProbeUDPMaxPayloadSize` | 1400 | UDP max payload |
| `bandwidthProbeAckGrace` | 500 ms | post-train ACK wait |
| `bandwidthProbeAckEvery` | 16 | ACK every N frames |
| `bandwidthProbeFrameOverhead` | 30 | frame header bytes |

## Constants Removed

- `bandwidthProbePacingGainNum` / `bandwidthProbePacingGainDen`
- `bandwidthProbeGrowthMinNum` / `bandwidthProbeGrowthMinDen`
- `bandwidthProbeDeliveryMinNum` / `bandwidthProbeDeliveryMinDen`
- `bandwidthProbeRelativeMinTargetBps`
- `bandwidthProbeLossIncreaseEpsilon`
- `bandwidthProbeMultiplicativeChunks`
- None of the `lane_quality.go` constants are touched (they stay where they are)

## Functions Removed

- `shouldStopBandwidthProbeTrain` — replaced by inline per-leg checks
- `bandwidthProbeMateriallyBelowReference` — no longer needed
- `bandwidthProbeTargetReachedReference` — no longer needed
- `bandwidthProbeGrowthStalled` — replaced by local `plateau()`
- `bandwidthProbeUnderDelivered` — no longer needed
- `bandwidthProbeRelativeTargetMature` — no longer needed
- `bandwidthProbeLossIncreased` — no longer needed
- `bandwidthProbePreviousStepAckedBytes` — no longer needed
- `bandwidthProbeStepBps` — unused, removed
- `bandwidthProbeBestCompletedStepBps` — replaced by max over steps
- `bandwidthProbeCandidate` — simplified inline

## Candidate Selection (unchanged logic)

TCP is always preferred when it needs probing. UDP only starts when
TCP reference has `ProbeSamples >= 1` and UDP is unprobed. If no TCP
leg exists, UDP probes without a reference (cap = 0).

## Completing the Train

```go
completeTrain(state):
    // Best step BPS across all completed steps
    refBps = max(stepBps for each step)
    aggregateLoss = (totalSent - totalAcked) / totalSent

    if leg == UDP && cap > 0:
        // QoS decision: did UDP fail to approach TCP reference?
        qosLimited = aggregateLoss >= 1% or refBps < cap * 0.67

    lane.recordBandwidthSample(kind, refBps, aggregateLoss)
```

## Trade-offs

- **No multiplicative gain**: The old code had it disabled
  (`MultiplicativeChunks = 0`). Adaptive doubling for far-from-cap and
  small additive steps near-cap is both simpler and more predictable.
- **Two stop conditions for UDP instead of one**: The user originally
  wanted only loss-based stop, but plateau-below-cap was added to
  avoid waiting for bufferbloat overflow. This is pragmatic.
- **TCP no hard window**: Plateau stops TCP immediately. Safety cap
  at 30 s prevents infinite probing on links with continuously growing
  capacity. In practice plateau fires well before 30 s.
- **External API unchanged**: Full internal rewrite with zero
  interface breakage to RecvState and ProbeLoop callers.

## Testing

All existing `bandwidth_probe_test.go` test functions must be rewritten
to match the new internal structure. New test cases:

| Test | Description |
|------|-------------|
| TCP plateaus and stops early | TCP stepBps growth < 5%, train stops before window |
| TCP keeps growing | TCP stepBps grows each step, train eventually plateaus |
| UDP stops on loss | UDP loss >= 1%, train stops immediately |
| UDP plateaus below cap | UDP stepBps flat while rate < cap, train stops (QoS) |
| UDP reaches cap, no loss, no plateau | UDP hits cap, stable, runs to window end |
| UDP rate never exceeds cap | Verify nextRate never returns > cap for UDP |
| UDP starts at ref/4 | Verify start rate = max(16M, ref/4) |
| Adaptive rate near cap | Verify small steps when close to cap |
| Adaptive rate far from cap | Verify doubling when far from cap |
| TCP rate +10Mbps each step | TCP with no plateau adds 10Mbps each step |
