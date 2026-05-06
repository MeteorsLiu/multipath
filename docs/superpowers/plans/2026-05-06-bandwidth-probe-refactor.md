# Bandwidth Probe Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `bandwidth_probe.go` to structurally guarantee UDP rate never exceeds TCP reference, with simplified stop conditions and adaptive rate advance.

**Architecture:** Two separate probe strategies — TCP adds +10Mbps per step until throughput plateaus (establishing the reference), UDP adaptive-advances toward the TCP reference cap and stops on loss or plateau-below-cap. All external call signatures unchanged.

**Tech Stack:** Go, `golang.org/x/time/rate`, internal protocol/transport packages.

---

### Task 1: Strip dead constants

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go:18-40`

- [ ] **Step 1: Replace constants block**

The constants block (lines 18-40) becomes:

```go
const (
	bandwidthProbeAckGrace          = 500 * time.Millisecond
	bandwidthProbeWindow            = 10 * time.Second
	bandwidthProbeRoundWindow       = 500 * time.Millisecond
	bandwidthProbeBurstWindow       = 2 * time.Millisecond
	bandwidthProbeUDPMinPayloadSize = 1200
	bandwidthProbeUDPMaxPayloadSize = 1400
	bandwidthProbeTCPPayloadSize    = 32 * 1024
	bandwidthProbeMaxFrames         = 64
	bandwidthProbeAckEvery          = 16
	bandwidthProbeFrameOverhead     = 30
	bandwidthProbeMinRateBps        = uint64(16_000_000)
	bandwidthProbeAdditiveStepBps   = uint64(10_000_000)
	bandwidthProbePlateauGrowth     = 1.05
	bandwidthProbePlateauSteps      = 2
	bandwidthProbeTCPSafetyWindow   = 30 * time.Second
	bandwidthProbeTCPRateBps        = uint64(16_000_000)
)
```

Removed: PacingGainNum/Den, GrowthMinNum/Den, DeliveryMinNum/Den, RelativeMinTargetBps, LossIncreaseEpsilon, MultiplicativeChunks.

- [ ] **Step 2: Build check**

```bash
go build ./internal/tunnel/send/
```

Expected: import errors from removed constants (fix in subsequent tasks).

- [ ] **Step 3: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: strip dead constants from bandwidth probe"
```

---

### Task 2: Add plateau and nextRate helpers

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Add `bandwidthProbePlateau` function**

Insert after the constants block, before line 42:

```go
func bandwidthProbePlateau(stepBps []uint64) bool {
	if len(stepBps) < bandwidthProbePlateauSteps+1 {
		return false
	}
	n := len(stepBps)
	lastMax := stepBps[n-1]
	if stepBps[n-2] > lastMax {
		lastMax = stepBps[n-2]
	}
	prev := stepBps[n-1-bandwidthProbePlateauSteps]
	return float64(lastMax) < float64(prev)*bandwidthProbePlateauGrowth
}
```

- [ ] **Step 2: Add `nextBandwidthProbeRate` function**

Insert after `bandwidthProbePlateau`:

```go
func nextBandwidthProbeRate(rate, cap uint64, growthStalled bool) uint64 {
	if cap > 0 && rate >= cap {
		return cap
	}
	if cap == 0 {
		if growthStalled {
			return rate
		}
		return rate + bandwidthProbeAdditiveStepBps
	}
	gap := cap - rate
	if gap > rate {
		return rate * 2
	}
	if gap > rate/4 {
		step := gap / 2
		if step < bandwidthProbeAdditiveStepBps {
			step = bandwidthProbeAdditiveStepBps
		}
		next := rate + step
		if next > cap {
			return cap
		}
		return next
	}
	next := rate + 5_000_000
	if next > cap {
		return cap
	}
	return next
}
```

- [ ] **Step 3: Build check**

```bash
go build ./internal/tunnel/send/
```

Expected: succeeds (new functions don't break anything).

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: add plateau and nextRate helpers for bandwidth probe"
```

---

### Task 3: Restructure bandwidthLegState

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go:42-67`

- [ ] **Step 1: Replace `bandwidthLegState` struct**

Replace lines 42-67 with:

```go
type bandwidthLegState struct {
	key      laneKey
	capBps   uint64
	rateBps  uint64

	inFlight  bool
	complete  bool
	startedAt time.Time
	endedAt   time.Time

	sentFrames  uint64
	ackedFrames uint64
	ackedBytes  uint64

	steps     map[uint64]*bandwidthProbeStep
	stepOrder []uint64
	stepBps   []uint64

	lastLoss float64
}
```

Removed: `nextRateBps`, `ewmaBps`, `sampleCount`, `rampChunks`, `prevStepBytes`, `prevStepLoss`, `lastStepLoss`, `lastStepID`, `currentStepID`, `stepStartedAt`, `lastStepBps`, `lastStepBytes`, `maxStepBps`, `lastRoundLoss`.

Fields still needed but surfaced differently:
- `rateBps` replaces `nextRateBps` for current rate
- `capBps` is the TCP reference cap (0 for TCP legs)
- `stepOrder` / `stepBps` arrays feed plateau detection

- [ ] **Step 2: Add `bandwidthProbeStep` struct (keep existing, lines 69-80 unchanged)**

No changes needed to this type.

- [ ] **Step 3: Fix compilation — update all references**

Search and replace across the file:
- `state.nextRateBps` → `state.rateBps`
- Add `state.capBps` initialization where probe starts
- Remove all references to deleted fields

```bash
go build ./internal/tunnel/send/ 2>&1
```

Fix each compilation error. Key locations:
- `maybeStartBandwidthProbe`: `state.nextRateBps` → `state.rateBps`
- `startBandwidthProbeRound`: `state.nextRateBps` → `state.rateBps`
- `advanceBandwidthProbeRate`: rewrite completely (Task 4)
- `completeBandwidthProbeTrain`: rewrite completely (Task 7)
- `startBandwidthProbeStep`: use `rateBps` instead of `nextRateBps`
- `finishBandwidthProbeStep`: simplify
- `receiveBandwidthProbeAck`: adapt

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: simplify bandwidthLegState struct"
```

---

### Task 4: Rewrite rate advance and remove shouldStop

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Rewrite `advanceBandwidthProbeRate`**

Replace the entire function body (lines 808-846) with:

```go
func (l *Send) advanceBandwidthProbeRate(legKey pingKey) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete {
		return
	}
	if state.rateBps == 0 {
		state.rateBps = bandwidthProbeMinRateBps
	}
	growthStalled := bandwidthProbePlateau(state.stepBps)
	state.rateBps = nextBandwidthProbeRate(state.rateBps, state.capBps, growthStalled)
}
```

- [ ] **Step 2: Delete `shouldStopBandwidthProbeTrain` and all 6 stop-condition helpers**

Remove these functions:
- `shouldStopBandwidthProbeTrain` (line 347)
- `bandwidthProbeMateriallyBelowReference` (line 413)
- `bandwidthProbeTargetReachedReference` (line 420)
- `bandwidthProbeGrowthStalled` (line 890)
- `bandwidthProbeUnderDelivered` (line 897)
- `bandwidthProbeRelativeTargetMature` (line 904)
- `bandwidthProbeLossIncreased` (line 908)
- `bandwidthProbePreviousStepAckedBytes` (line 848)
- `bandwidthProbeStepBps` (line 860)

Also remove `updateBandwidthProbeStepSample` and `refreshBandwidthProbeStepSample` (lines 785-806) — step sample updating is now done inline in `finishBandwidthProbeStep`.

- [ ] **Step 3: Build check and fix compilation**

```bash
go build ./internal/tunnel/send/ 2>&1
```

Expected: errors in `runBandwidthProbeTrain` (uses deleted functions).

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: rewrite rate advance, remove stop condition helpers"
```

---

### Task 5: Rewrite probe train loop

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Rewrite `runBandwidthProbeTrain`**

Replace function (lines 296-345) with:

```go
func (l *Send) runBandwidthProbeTrain(ctx context.Context, key laneKey, leg transport.LegRef, legKey pingKey) {
	isUDP := leg.Kind == transport.KindUDP
	window := bandwidthProbeWindow
	if !isUDP {
		window = bandwidthProbeTCPSafetyWindow
	}
	deadline := time.Now().Add(window)
	endedAt := deadline

	var limiterState bandwidthProbeLimiterState
	var stepID uint64

	for time.Now().Before(deadline) {
		now := time.Now()
		stepDeadline := now.Add(bandwidthProbeRoundWindow)
		if stepDeadline.After(deadline) {
			stepDeadline = deadline
		}
		stepID++
		step := l.startBandwidthProbeStep(legKey, stepID, now)
		if step == nil {
			return
		}
		for time.Now().Before(stepDeadline) {
			round := l.startBandwidthProbeRound(key, leg, legKey, stepID, time.Now())
			if round == nil {
				return
			}
			limiter := limiterState.forRound(leg, round)
			sent := l.runBandwidthProbeRound(ctx, round, stepDeadline, limiter)
			l.recordBandwidthProbeSent(round, sent)
			if sent < round.count {
				if ctx.Err() != nil || time.Now().Before(deadline) && time.Now().Before(stepDeadline) {
					l.abortBandwidthProbeTrain(legKey)
					return
				}
				break
			}
		}
		stepBps, stepLoss := l.finishBandwidthProbeStep(legKey, stepID, stepDeadline)
		if stepBps == 0 && stepLoss >= 1 {
			l.abortBandwidthProbeTrain(legKey)
			return
		}

		if isUDP {
			l.bandwidthMu.Lock()
			state := l.bandwidthLegs[legKey]
			capBps := state.capBps
			rateBps := state.rateBps
			l.bandwidthMu.Unlock()

			if stepLoss >= bandwidthProbeLossThreshold {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_udp_loss session=%d lane=%d loss=%.3f", key.sessionID, key.laneID, stepLoss)
				break
			}
			if capBps > 0 && rateBps < capBps && bandwidthProbePlateau(l.getStepBpsSlice(legKey)) {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_udp_plateau_below_cap session=%d lane=%d", key.sessionID, key.laneID)
				break
			}
		} else {
			if bandwidthProbePlateau(l.getStepBpsSlice(legKey)) {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_tcp_plateau session=%d lane=%d", key.sessionID, key.laneID)
				break
			}
		}

		l.advanceBandwidthProbeRate(legKey)
	}

	l.markBandwidthProbeSendComplete(legKey, endedAt)

	timer := time.NewTimer(bandwidthProbeAckGrace)
	select {
	case <-ctx.Done():
		timer.Stop()
		l.abortBandwidthProbeTrain(legKey)
		return
	case <-timer.C:
	}
	l.completeBandwidthProbeTrain(key, leg, legKey)
}
```

- [ ] **Step 2: Add `getStepBpsSlice` accessor**

Add to bandwidth_probe.go:

```go
func (l *Send) getStepBpsSlice(legKey pingKey) []uint64 {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		return nil
	}
	return state.stepBps
}
```

- [ ] **Step 3: Build check**

```bash
go build ./internal/tunnel/send/ 2>&1
```

Fix any compilation errors.

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: rewrite probe train loop with new stop logic"
```

---

### Task 6: Adapt step and round functions

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Rewrite `startBandwidthProbeStep`**

Replace (lines 742-757) with:

```go
func (l *Send) startBandwidthProbeStep(legKey pingKey, stepID uint64, now time.Time) *bandwidthProbeStep {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || !state.inFlight || state.complete {
		return nil
	}
	if state.steps == nil {
		state.steps = make(map[uint64]*bandwidthProbeStep)
	}
	step := &bandwidthProbeStep{
		startedAt: now,
		rateBps:   state.rateBps,
	}
	state.steps[stepID] = step
	state.stepOrder = append(state.stepOrder, stepID)
	return step
}
```

- [ ] **Step 2: Rewrite `finishBandwidthProbeStep`**

Replace (lines 759-783) with:

```go
func (l *Send) finishBandwidthProbeStep(legKey pingKey, stepID uint64, endedAt time.Time) (uint64, float64) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete {
		return 0, 1
	}
	step := state.steps[stepID]
	if step == nil {
		return 0, 1
	}
	step.endedAt = endedAt
	stepBps := bandwidthProbeStepCeilingBps(step)
	stepLoss := bandwidthAggregateLoss(stepSentFrames(step), stepAckedFrames(step))
	state.stepBps = append(state.stepBps, stepBps)
	return stepBps, stepLoss
}
```

- [ ] **Step 3: Adapt `startBandwidthProbeRound`**

In `startBandwidthProbeRound` (line 459), change the function signature to accept `stepID uint64` instead of using `state.currentStepID`:

Replace the function signature and body at lines 459-493 with:

```go
func (l *Send) startBandwidthProbeRound(key laneKey, leg transport.LegRef, legKey pingKey, stepID uint64, now time.Time) *bandwidthProbeRound {
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil || !state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return nil
	}
	rateBps := state.rateBps
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	probeID := l.nextBWProbeID.Add(1)
	payloadBytes := bandwidthProbeUDPPayloadSize(key, probeID, rateBps)
	if leg.Kind == transport.KindTCP {
		payloadBytes = bandwidthProbeTCPPayloadSize
	}
	frameBytes := bandwidthProbeFrameBytes(payloadBytes)
	round := &bandwidthProbeRound{
		key:          key,
		leg:          leg,
		legKey:       legKey,
		stepID:       stepID,
		probeID:      probeID,
		count:        probeFrameCount(rateBps, frameBytes),
		payloadBytes: payloadBytes,
		frameBytes:   frameBytes,
		rateBps:      rateBps,
		startedAt:    now,
	}
	l.bandwidthPending[probeID] = round
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "round_start session=%d lane=%d leg={%s} probe_id=%d rate_bps=%d count=%d payload=%d frame_bytes=%d", key.sessionID, key.laneID, debugLeg(leg), probeID, rateBps, round.count, payloadBytes, frameBytes)
	return round
}
```

- [ ] **Step 4: Build check**

```bash
go build ./internal/tunnel/send/ 2>&1
```

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: adapt step and round functions"
```

---

### Task 7: Rewrite completion logic

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Rewrite `completeBandwidthProbeTrain`**

Replace (lines 934-990) with:

```go
func (l *Send) completeBandwidthProbeTrain(key laneKey, leg transport.LegRef, legKey pingKey) bool {
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		l.bandwidthMu.Unlock()
		return false
	}
	now := time.Now()
	if state.startedAt.IsZero() {
		state.startedAt = now.Add(-bandwidthProbeWindow)
	}
	if state.endedAt.IsZero() {
		state.endedAt = now
	}
	var bestBps uint64
	for _, stepID := range state.stepOrder {
		if step := state.steps[stepID]; step != nil && !step.endedAt.IsZero() {
			if bps := bandwidthProbeStepCeilingBps(step); bps > bestBps {
				bestBps = bps
			}
		}
	}
	if bestBps == 0 {
		bestBps = bandwidthWindowSampleBps(state.ackedBytes, state.startedAt, state.endedAt)
	}
	aggregateLoss := bandwidthAggregateLoss(state.sentFrames, state.ackedFrames)
	state.lastLoss = aggregateLoss
	state.complete = true
	state.inFlight = false
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
		}
	}
	l.bandwidthMu.Unlock()

	if lane := l.getLane(key); lane != nil {
		lane.recordBandwidthSample(leg.Kind, bestBps, aggregateLoss)
	}
	metrics.SetGauge(metrics.LaneBandwidthBps, float64(bestBps),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	metrics.SetGauge(metrics.LaneProbeLossRatio, aggregateLoss,
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	metrics.IncCounter(metrics.BandwidthProbeEventsTotal,
		metrics.L("event", "finish"),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	l.logBandwidthProbeDecisionIfReady(key)
	debuglog.Printf("send/bw_probe", "train_finish session=%d lane=%d leg={%s} loss=%.3f bps=%d", key.sessionID, key.laneID, debugLeg(leg), aggregateLoss, bestBps)
	return true
}
```

- [ ] **Step 2: Simplify `logBandwidthProbeDecisionIfReady`**

Replace (lines 1022-1046) with:

```go
func (l *Send) logBandwidthProbeDecisionIfReady(key laneKey) {
	lane := l.getLane(key)
	if lane == nil {
		return
	}
	udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
	if udpQ.ProbeSamples < minBandwidthProbeSamples || tcpQ.ProbeSamples < minBandwidthProbeSamples {
		return
	}
	useUDP, ok := l.legSelector(key.sessionID).Pick(udpQ, tcpQ)
	logBandwidthProbeDecision(key.sessionID, key.laneID, udpQ, tcpQ, useUDP, ok)
}
```

Removed: `bandwidthMu` lock on `bandwidthLegs` lookup — the decision now comes purely from lane quality data (already available via `legQualities()`).

- [ ] **Step 3: Remove `bandwidthProbeBestCompletedStepBps`**

This function is no longer used (logic inlined in `completeBandwidthProbeTrain`).

- [ ] **Step 4: Build check**

```bash
go build ./internal/tunnel/send/ 2>&1
```

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: rewrite completion and decision logging"
```

---

### Task 8: Adapt ACK and sent recording

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Adapt `recordBandwidthProbeSent`**

The function (line 710) uses `state.steps[round.stepID]` — this is fine, stepID is now passed from the caller. But remove the rateBps setting (step rate is set in `startBandwidthProbeStep` now). The function body stays mostly the same.

Replace lines 710-740:

```go
func (l *Send) recordBandwidthProbeSent(round *bandwidthProbeRound, sent uint16) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	if sent == 0 {
		delete(l.bandwidthPending, round.probeID)
		return
	}
	state := l.bandwidthLegs[round.legKey]
	if state == nil {
		state = &bandwidthLegState{}
		l.bandwidthLegs[round.legKey] = state
	}
	state.sentFrames += uint64(sent)
	if state.steps == nil {
		state.steps = make(map[uint64]*bandwidthProbeStep)
	}
	step := state.steps[round.stepID]
	if step == nil {
		step = &bandwidthProbeStep{startedAt: round.startedAt, rateBps: round.rateBps}
		state.steps[round.stepID] = step
	}
	if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
		step.startedAt = round.startedAt
	}
	step.sentBytes += uint64(sent) * uint64(round.frameBytes)
	step.sentFrames += uint64(sent)
}
```

- [ ] **Step 2: Adapt `receiveBandwidthProbeAck`**

The ACK processing (line 648) needs the step sample update logic inline instead of calling `updateBandwidthProbeStepSample`. Replace lines 648-708:

```go
func (l *Send) receiveBandwidthProbeAck(sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.BandwidthProbeAckBody) error {
	legKey := newPingKey(leg)
	l.bandwidthMu.Lock()
	round := l.bandwidthPending[body.ProbeID]
	if round == nil || round.key.sessionID != sessionID || round.key.laneID != laneID || round.legKey != legKey || round.count != body.Count {
		l.bandwidthMu.Unlock()
		debuglog.Printf("send/bw_probe", "ack_drop stale session=%d lane=%d leg={%s} probe_id=%d count=%d", sessionID, laneID, debugLeg(leg), body.ProbeID, body.Count)
		return nil
	}
	prev := round.received
	round.received |= body.Received
	if body.FirstRXMS != 0 && (round.firstRXMS == 0 || body.FirstRXMS < round.firstRXMS) {
		round.firstRXMS = body.FirstRXMS
	}
	if body.LastRXMS > round.lastRXMS {
		round.lastRXMS = body.LastRXMS
	}
	received := round.received
	var full uint64
	if round.count == 64 {
		full = ^uint64(0)
	} else {
		full = (uint64(1) << round.count) - 1
	}
	if received == full {
		delete(l.bandwidthPending, body.ProbeID)
	}
	newBits := received &^ prev
	if newBits != 0 {
		if state := l.bandwidthLegs[legKey]; state != nil && !state.complete {
			acked := bits.OnesCount64(newBits)
			ackedBytes := uint64(acked) * uint64(round.frameBytes)
			state.ackedFrames += uint64(acked)
			state.ackedBytes += ackedBytes
			if state.steps == nil {
				state.steps = make(map[uint64]*bandwidthProbeStep)
			}
			step := state.steps[round.stepID]
			if step == nil {
				step = &bandwidthProbeStep{startedAt: round.startedAt, rateBps: round.rateBps}
				state.steps[round.stepID] = step
			}
			if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
				step.startedAt = round.startedAt
			}
			updateBandwidthProbeStepRXSpan(step, round)
			step.ackedBytes += ackedBytes
			step.ackedFrames += uint64(acked)
		}
	}
	l.bandwidthMu.Unlock()
	return nil
}
```

Key change: removed `updateBandwidthProbeStepSample` call at line 699 and inline step field updates in the state. Removed `lastRoundLoss` update from state.

- [ ] **Step 3: Build check**

```bash
go build ./internal/tunnel/send/ 2>&1
```

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: adapt ACK processing and sent recording"
```

---

### Task 9: Adapt probe initialization (cap and start rate)

**Files:**
- Modify: `internal/tunnel/send/bandwidth_probe.go`

- [ ] **Step 1: Add `udpBandwidthProbeRateCap` helper**

Insert before `bandwidthProbeTCPReferenceBps`:

```go
func (l *Send) udpBandwidthProbeRateCap(key laneKey, leg transport.LegRef) uint64 {
	if leg.Kind != transport.KindUDP {
		return 0
	}
	return l.bandwidthProbeTCPReferenceBps(key)
}
```

- [ ] **Step 2: Adapt `maybeStartBandwidthProbe`**

In `maybeStartBandwidthProbe` (line 237), after setting up the state, compute cap and set start rate. Replace lines 237-282:

```go
func (l *Send) maybeStartBandwidthProbe(ctx context.Context, key laneKey, leg transport.LegRef, now time.Time) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}

	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		state = &bandwidthLegState{key: key}
		l.bandwidthLegs[legKey] = state
	}
	state.key = key
	if state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return
	}
	state.inFlight = true
	state.startedAt = now
	state.endedAt = time.Time{}
	state.sentFrames = 0
	state.ackedFrames = 0
	state.ackedBytes = 0
	state.steps = make(map[uint64]*bandwidthProbeStep)
	state.stepOrder = nil
	state.stepBps = nil
	state.lastLoss = 0
	state.capBps = l.udpBandwidthProbeRateCap(key, leg)
	state.rateBps = bandwidthProbeStartRate(state.capBps)
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "train_start session=%d lane=%d leg={%s} cap_bps=%d start_bps=%d window=%s", key.sessionID, key.laneID, debugLeg(leg), state.capBps, state.rateBps, bandwidthProbeWindow)
	go l.runBandwidthProbeTrain(ctx, key, leg, legKey)
}
```

- [ ] **Step 3: Add `bandwidthProbeStartRate` helper**

Add to the file:

```go
func bandwidthProbeStartRate(capBps uint64) uint64 {
	if capBps == 0 {
		return bandwidthProbeMinRateBps
	}
	start := capBps / 4
	if start < bandwidthProbeMinRateBps {
		start = bandwidthProbeMinRateBps
	}
	if start > capBps {
		start = capBps
	}
	return start
}
```

- [ ] **Step 4: Build check**

```bash
go build ./internal/tunnel/send/ 2>&1
```

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe.go
git commit -m "refactor: add cap and adaptive start rate to probe initialization"
```

---

### Task 10: Full test rewrite

**Files:**
- Rewrite: `internal/tunnel/send/bandwidth_probe_test.go`

- [ ] **Step 1: Delete existing test file and write new tests**

The test file is 970 lines. Replace it entirely with tests for the new behavior:

```go
package send

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func mustUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	u, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func udpLeg() transport.LegRef {
	return transport.LegRef{
		Kind:       transport.KindUDP,
		EndpointID: "udp0",
		RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
	}
}

func tcpLeg() transport.LegRef {
	return transport.LegRef{
		Kind:       transport.KindTCP,
		EndpointID: "tcp0",
		RemoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678},
	}
}

func TestBandwidthProbePlateau(t *testing.T) {
	// Growth stalled
	if !bandwidthProbePlateau([]uint64{100, 100, 100}) {
		t.Fatal("want plateau for flat samples")
	}
	// Growing
	if bandwidthProbePlateau([]uint64{100, 110, 120}) {
		t.Fatal("want no plateau for growing samples")
	}
	// Edge: too few samples
	if bandwidthProbePlateau([]uint64{100, 100}) {
		t.Fatal("want no plateau with 2 samples")
	}
	// Slack: max of last 2 is 105, prev is 100, 105 < 105 = no plateau
	if bandwidthProbePlateau([]uint64{100, 100, 105}) {
		t.Fatal("want no plateau at growth boundary")
	}
	// Stalled: max of last 2 is 104, prev is 100, 104 <= 105 = plateau
	if !bandwidthProbePlateau([]uint64{100, 100, 104}) {
		t.Fatal("want plateau just below growth threshold")
	}
}

func TestNextBandwidthProbeRateUDPAdaptive(t *testing.T) {
	cap := uint64(100_000_000)

	// Far from cap: double
	if got := nextBandwidthProbeRate(25_000_000, cap, false); got != 50_000_000 {
		t.Fatalf("far from cap: got %d, want 50000000", got)
	}
	// Medium gap: + gap/2
	if got := nextBandwidthProbeRate(50_000_000, cap, false); got != 75_000_000 {
		t.Fatalf("medium gap: got %d, want 75000000", got)
	}
	// Close to cap: small step
	next := nextBandwidthProbeRate(90_000_000, cap, false)
	if next > cap || next <= 90_000_000 {
		t.Fatalf("close to cap: got %d, want between 90M and %d", next, cap)
	}
	// At cap: no advance
	if got := nextBandwidthProbeRate(cap, cap, false); got != cap {
		t.Fatalf("at cap: got %d, want %d", got, cap)
	}
	// Above cap: clamp
	if got := nextBandwidthProbeRate(120_000_000, cap, false); got != cap {
		t.Fatalf("above cap: got %d, want %d", got, cap)
	}
}

func TestNextBandwidthProbeRateTCPNoCap(t *testing.T) {
	// TCP additive +10Mbps per step (no plateau)
	if got := nextBandwidthProbeRate(16_000_000, 0, false); got != 26_000_000 {
		t.Fatalf("TCP no plateau: got %d, want 26000000", got)
	}
	// TCP plateau: no change
	if got := nextBandwidthProbeRate(128_000_000, 0, true); got != 128_000_000 {
		t.Fatalf("TCP plateau: got %d, want 128000000", got)
	}
}

func TestBandwidthProbeStartRate(t *testing.T) {
	if got := bandwidthProbeStartRate(0); got != bandwidthProbeMinRateBps {
		t.Fatalf("no cap: got %d, want %d", got, bandwidthProbeMinRateBps)
	}
	if got := bandwidthProbeStartRate(100_000_000); got != 25_000_000 {
		t.Fatalf("100M cap: got %d, want 25000000", got)
	}
	if got := bandwidthProbeStartRate(50_000_000); got != 16_000_000 {
		t.Fatalf("50M cap (ref/4 < min): got %d, want 16000000", got)
	}
	if got := bandwidthProbeStartRate(4_000_000); got != 4_000_000 {
		t.Fatalf("4M cap (cap < min): got %d, want 4000000", got)
	}
}

func TestBandwidthProbeLimiterFromRate(t *testing.T) {
	limiter := newBandwidthProbeLimiter(bandwidthProbeMinRateBps, bandwidthProbeFrameBytes(bandwidthProbeUDPMinPayloadSize))
	if limiter == nil {
		t.Fatal("missing limiter")
	}
}

func TestBandwidthProbeFrameEncoding(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}
	leg := udpLeg()
	legKey := newPingKey(leg)

	in.bandwidthLegs[legKey] = &bandwidthLegState{
		key:     key,
		rateBps: bandwidthProbeMinRateBps,
		inFlight: true,
		steps:   make(map[uint64]*bandwidthProbeStep),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	round := in.startBandwidthProbeRound(key, leg, legKey, 1, time.Now())
	if round == nil {
		t.Fatal("nil round")
	}
	if round.count == 0 {
		t.Fatal("round count is 0")
	}
	if round.payloadBytes == 0 {
		t.Fatal("payload bytes is 0")
	}
}

func TestBandwidthProbeUDPRateCap(t *testing.T) {
	in := New()
	key := laneKey{sessionID: 99, laneID: 3}

	// No lane, no cap
	if got := in.udpBandwidthProbeRateCap(key, udpLeg()); got != 0 {
		t.Fatalf("no lane: got %d, want 0", got)
	}
	// TCP leg, no cap
	if got := in.udpBandwidthProbeRateCap(key, tcpLeg()); got != 0 {
		t.Fatalf("TCP leg: got %d, want 0", got)
	}
}

func TestProbeFrameCount(t *testing.T) {
	count := probeFrameCount(16_000_000, bandwidthProbeFrameBytes(1400))
	if count < 2 || count > bandwidthProbeMaxFrames {
		t.Fatalf("count=%d, want [2,%d]", count, bandwidthProbeMaxFrames)
	}
	count = probeFrameCount(1_000_000_000, bandwidthProbeFrameBytes(bandwidthProbeTCPPayloadSize))
	if count != bandwidthProbeMaxFrames {
		t.Fatalf("count=%d, want capped at %d", count, bandwidthProbeMaxFrames)
	}
}
```

- [ ] **Step 2: Run tests**

```bash
go test ./internal/tunnel/send/ -run 'TestBandwidthProbe|TestNextBandwidth|TestProbeFrameCount' -v -count=1
```

Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/tunnel/send/bandwidth_probe_test.go
git commit -m "test: rewrite bandwidth probe tests for refactored module"
```

---

### Task 11: Final build and test verification

- [ ] **Step 1: Full build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 2: Full test run**

```bash
go test ./internal/tunnel/send/ -v -count=1 2>&1
```

Expected: all tests pass. Fix any failures.

- [ ] **Step 3: Run all project tests**

```bash
go test ./... 2>&1 | tail -20
```

Expected: all tests pass.

- [ ] **Step 4: Commit final state**

```bash
git add -A
git diff --cached --stat
git commit -m "refactor: complete bandwidth probe module rewrite"
```
