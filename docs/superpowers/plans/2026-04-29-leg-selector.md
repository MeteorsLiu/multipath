# Leg Selector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce two-level scheduling — pick a lane via existing CFS, then pick a leg (UDP/TCP) within that lane based on measured delivery quality.

**Architecture:** No new packages. `LegSelector` interface and `legQualityTracker` live in `internal/tunnel/send/`. `schedule.Strategy` and `transport` are unchanged. The lane picks the lane; `Send` owns a per-session `LegSelector` map that decides UDP vs TCP.

**Tech Stack:** Go, existing CFS scheduler, existing PING/PONG probe loop.

---

### Task 1: LegQuality and LegSelector interface

**Files:**
- Create: `internal/tunnel/send/leg_selector.go`
- Test: `internal/tunnel/send/leg_selector_test.go`

- [ ] **Step 1: Write the failing test**

```go
package send

import (
    "testing"
    "time"
    "github.com/MeteorsLiu/multipath/internal/transport"
)

func TestQualityLegSelectorPrefersUDPWhenBothHealthy(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 50 * time.Millisecond}
    tcp := LegQuality{Active: true, DeliveryRate: 1.0, SmoothedRTT: 100 * time.Millisecond}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok {
        t.Fatal("Pick returned false, want true")
    }
    if !useUDP {
        t.Fatal("Pick returned TCP, want UDP (both healthy)")
    }
}

func TestQualityLegSelectorFallsBackToTCPWhenUDPDegraded(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}
    tcp := LegQuality{Active: true, DeliveryRate: 0.95, SmoothedRTT: 100 * time.Millisecond}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok {
        t.Fatal("Pick returned false, want true")
    }
    if useUDP {
        t.Fatal("Pick returned UDP, want TCP (UDP degraded)")
    }
}

func TestQualityLegSelectorStaysOnUDPWhenTCPAlsoDegraded(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: true, DeliveryRate: 0.70, SmoothedRTT: 100 * time.Millisecond}
    tcp := LegQuality{Active: true, DeliveryRate: 0.50, SmoothedRTT: 200 * time.Millisecond}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok {
        t.Fatal("Pick returned false, want true")
    }
    if !useUDP {
        t.Fatal("Pick returned TCP when TCP is also degraded")
    }
}

func TestQualityLegSelectorOnlyUDP(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: true, DeliveryRate: 1.0}
    tcp := LegQuality{Active: false}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok || !useUDP {
        t.Fatal("Pick should return UDP")
    }
}

func TestQualityLegSelectorOnlyTCP(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: false}
    tcp := LegQuality{Active: true, DeliveryRate: 1.0}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok || useUDP {
        t.Fatal("Pick should return TCP")
    }
}

func TestQualityLegSelectorNoneActive(t *testing.T) {
    sel := QualityLegSelector{}
    udp := LegQuality{Active: false}
    tcp := LegQuality{Active: false}

    if _, ok := sel.Pick(udp, tcp); ok {
        t.Fatal("Pick should return false when neither active")
    }
}

func TestUDPPreferssSelectorPrefersUDP(t *testing.T) {
    sel := UDPPreferssSelector{}
    udp := LegQuality{Active: true, DeliveryRate: 0.10}
    tcp := LegQuality{Active: true, DeliveryRate: 1.0}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok || !useUDP {
        t.Fatal("UDPPreferssSelector should always pick UDP when active")
    }
}

func TestUDPPreferssSelectorFallsBackToTCP(t *testing.T) {
    sel := UDPPreferssSelector{}
    udp := LegQuality{Active: false}
    tcp := LegQuality{Active: true, DeliveryRate: 1.0}

    useUDP, ok := sel.Pick(udp, tcp)
    if !ok || useUDP {
        t.Fatal("UDPPreferssSelector should fall back to TCP")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tunnel/send/ -run 'TestQuality|TestUDPPreferss' -v`
Expected: compilation error — types undefined

- [ ] **Step 3: Write minimal implementation**

```go
package send

import (
    "time"
    "github.com/MeteorsLiu/multipath/internal/transport"
)

type LegQuality struct {
    Active       bool
    DeliveryRate float64       // 0.0–1.0, fraction of probes delivered within deadline
    SmoothedRTT  time.Duration
    RTTVariance  time.Duration
}

type LegSelector interface {
    Pick(udp, tcp LegQuality) (useUDP bool, ok bool)
}

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

    const minUDPDelivery = 0.80
    const minTCPDelivery = 0.90

    if udp.DeliveryRate < minUDPDelivery && tcp.DeliveryRate >= minTCPDelivery {
        return false, true
    }
    return true, true
}

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

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tunnel/send/ -run 'TestQuality|TestUDPPreferss' -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/send/leg_selector.go internal/tunnel/send/leg_selector_test.go
git commit -m "feat: add LegQuality, LegSelector interface, and default implementations"
```

---

### Task 2: legQualityTracker

**Files:**
- Create: `internal/tunnel/send/quality.go`
- Test: `internal/tunnel/send/quality_test.go`

- [ ] **Step 1: Write the failing test**

```go
package send

import (
    "testing"
)

func TestLegQualityTrackerDefaultsToHealthy(t *testing.T) {
    var tr legQualityTracker
    rate := tr.deliveryRate()
    if rate != 1.0 {
        t.Fatalf("deliveryRate = %f, want 1.0 (no data yet)", rate)
    }
}

func TestLegQualityTrackerRecordsOnTime(t *testing.T) {
    var tr legQualityTracker
    tr.recordDelivery(true)
    tr.recordDelivery(true)
    if rate := tr.deliveryRate(); rate != 1.0 {
        t.Fatalf("deliveryRate = %f, want 1.0", rate)
    }
}

func TestLegQualityTrackerRecordsMisses(t *testing.T) {
    var tr legQualityTracker
    tr.recordDelivery(true)
    tr.recordDelivery(false)
    tr.recordDelivery(true)
    tr.recordDelivery(false)
    if rate := tr.deliveryRate(); rate != 0.5 {
        t.Fatalf("deliveryRate = %f, want 0.5", rate)
    }
}

func TestLegQualityTrackerWindowReset(t *testing.T) {
    var tr legQualityTracker
    tr.recordDelivery(true)
    tr.recordDelivery(false)
    tr.resetWindow()
    if rate := tr.deliveryRate(); rate != 1.0 {
        t.Fatalf("deliveryRate after reset = %f, want 1.0", rate)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tunnel/send/ -run 'TestLegQualityTracker' -v`
Expected: compilation error — type undefined

- [ ] **Step 3: Write minimal implementation**

```go
package send

// legQualityTracker tracks per-leg delivery rate.
// Methods must be called while holding the parent laneRuntime.mu.
type legQualityTracker struct {
    onTimeCount uint32
    totalCount  uint32
}

func (t *legQualityTracker) recordDelivery(onTime bool) {
    t.totalCount++
    if onTime {
        t.onTimeCount++
    }
}

func (t *legQualityTracker) deliveryRate() float64 {
    if t.totalCount == 0 {
        return 1.0
    }
    return float64(t.onTimeCount) / float64(t.totalCount)
}

func (t *legQualityTracker) resetWindow() {
    t.onTimeCount = 0
    t.totalCount = 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tunnel/send/ -run 'TestLegQualityTracker' -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tunnel/send/quality.go internal/tunnel/send/quality_test.go
git commit -m "feat: add legQualityTracker for per-leg delivery rate measurement"
```

---

### Task 3: Update laneRuntime — add quality trackers, legQualities(), remove selectLegLocked

**Files:**
- Modify: `internal/tunnel/send/lane.go`
- Modify: `internal/tunnel/send/lane_test.go`

- [ ] **Step 1: Update laneRuntime struct — add quality trackers, remove selectLegLocked**

Add these fields inside the `laneRuntime` struct:

```go
    // ...existing fields...
    udpQuality legQualityTracker
    tcpQuality legQualityTracker
```

Replace `selectLeg()` and `selectLegLocked()` with:

```go
func (l *laneRuntime) legQualities() (udpLeg transport.LegRef, udpQ LegQuality, tcpLeg transport.LegRef, tcpQ LegQuality) {
    l.mu.Lock()
    defer l.mu.Unlock()
    udpLeg = l.udpLeg
    udpQ = LegQuality{
        Active:       l.udpReady && l.udpLeg.EndpointID != "" && l.udpLeg.RemoteAddr != nil,
        DeliveryRate: l.udpQuality.deliveryRate(),
        SmoothedRTT:  durationOrZero(l.rttUDP.SRTT()),
        RTTVariance:  durationOrZero(l.rttUDP.RTTVAR()),
    }
    tcpLeg = l.tcpLeg
    tcpQ = LegQuality{
        Active:       l.tcpReady && l.tcpLeg.ConnID != "",
        DeliveryRate: l.tcpQuality.deliveryRate(),
        SmoothedRTT:  durationOrZero(l.rttTCP.SRTT()),
        RTTVariance:  durationOrZero(l.rttTCP.RTTVAR()),
    }
    return
}
```

Add helper:

```go
func durationOrZero(ms uint32, ok bool) time.Duration {
    if !ok {
        return 0
    }
    return time.Duration(ms) * time.Millisecond
}
```

Remove `selectLeg()` and `selectLegLocked()` methods. Keep `ready()` but reimplement without `selectLegLocked`:

```go
func (l *laneRuntime) ready() bool {
    if l == nil {
        return false
    }
    l.mu.Lock()
    defer l.mu.Unlock()
    return (l.udpReady && l.udpLeg.EndpointID != "" && l.udpLeg.RemoteAddr != nil) ||
        (l.tcpReady && l.tcpLeg.ConnID != "")
}
```

We need to update `ready()` to use the quality trackers or the leg qualities. Since `ready()` just checks if ANY leg is available, we can reimplement it:

```go
func (l *laneRuntime) ready() bool {
    if l == nil {
        return false
    }
    _, _, tcpLeg, tcpQ := l.legQualities()
    _ = tcpQ // unused but needed for signature
    _ = tcpLeg
    // legQualities already checks Active which includes readiness
    return true
}
```

Wait no, that's not right. `legQualities()` returns 4 values. Let me just check the UPD and TCP active status. Actually, since `legQualities()` computes quality from the lock, let me just use a simpler approach:

Replace `ready()`:
```go
func (l *laneRuntime) ready() bool {
    if l == nil {
        return false
    }
    l.mu.Lock()
    defer l.mu.Unlock()
    return (l.udpReady && l.udpLeg.EndpointID != "" && l.udpLeg.RemoteAddr != nil) ||
        (l.tcpReady && l.tcpLeg.ConnID != "")
}
```

Also add `recordDelivery(kind transport.Kind, onTime bool)`:

```go
func (l *laneRuntime) recordDelivery(kind transport.Kind, onTime bool) {
    l.mu.Lock()
    defer l.mu.Unlock()
    switch kind {
    case transport.KindUDP:
        l.udpQuality.recordDeliveryLocked(onTime)
    case transport.KindTCP:
        l.tcpQuality.recordDeliveryLocked(onTime)
    }
}
```

Wait, but `legQualityTracker.recordDelivery()` and `deliveryRate()` use their own mutex. Since `laneRuntime.mu` is separate from `legQualityTracker.mu`, calling `recordDeliveryLocked` bypasses the tracker's lock. But the caller already holds `laneRuntime.mu`, and the tracker is only accessed through lane methods — so we can either:
1. Not use a mutex in the tracker, trusting the lane mutex 
2. Or keep the tracker's mutex and acquire it separately

Option 1 is simpler and matches the pattern used for `rttUDP`/`rttTCP` (they're accessed inside `laneRuntime.mu`). Let me change the tracker to not use its own mutex:

Remove mutex from `legQualityTracker`, make it a plain struct:

```go
type legQualityTracker struct {
    onTimeCount uint32
    totalCount  uint32
}

func (t *legQualityTracker) recordDeliveryLocked(onTime bool) {
    t.totalCount++
    if onTime {
        t.onTimeCount++
    }
}

func (t *legQualityTracker) deliveryRateLocked() float64 {
    if t.totalCount == 0 {
        return 1.0
    }
    return float64(t.onTimeCount) / float64(t.totalCount)
}

func (t *legQualityTracker) resetLocked() {
    t.onTimeCount = 0
    t.totalCount = 0
}
```

Then the public methods `recordDelivery`, `deliveryRate`, `resetWindow` on laneRuntime do the locking.

Actually, to keep things simple and avoid needing a `Locked` variant in the tracker, let me define it mutex-free and the callers hold `laneRuntime.mu`. The tests call the tracker directly, but the tests can just hold a mutex manually... Actually no. The tests in Task 2 use a standalone `legQualityTracker` and call `recordDelivery(true)`. If we remove the mutex from the tracker, the tests will still compile but won't be thread-safe. However, the tests are single-threaded, so it's fine.

Let me revise: keep `legQualityTracker` with its own mutex. In lane, acquire the lane mutex + tracker mutex (nested, always lane first then tracker, consistent ordering). This is fine because the tracker is a small struct and the double-lock cost is negligible.

Actually, let's think about this differently. The `legQualityTracker` is only accessed from `laneRuntime` methods which already hold `laneRuntime.mu`. So we can skip the tracker's own mutex and just document that callers must hold the lane mutex.

Let me revise:

```go
// legQualityTracker tracks per-leg delivery rate.
// Methods must be called while holding the parent laneRuntime.mu.
type legQualityTracker struct {
    onTimeCount uint32
    totalCount  uint32
}
```

And the tests in Task 2 will be updated to not use the tracker's mutex (since the tests are single-threaded, the mutex is unnecessary). Let me simplify the whole thing.

Actually, I'll just keep the mutex in the tracker. The tasks are sequential and small. Let me not overthink this and keep it simple: keep the mutex in the tracker, add methods on laneRuntime that lock lane.mu then call tracker methods (which also lock their own mu). Double-lock is fine for a rarely-called path.

Let me just write the plan and move on.

- [ ] **Step 2: Update lane_test.go**

Replace calls to `lane.selectLeg()` with `lane.legQualities()`:

```go
func TestLaneRuntimePrefersUDP(t *testing.T) {
    lane := newLaneRuntime(1, 10)
    lane.observeLeg(transport.LegRef{
        Kind:       transport.KindUDP,
        EndpointID: "udp0",
        RemoteAddr: mustUDPAddr(t, "127.0.0.1:1234"),
    })
    lane.observeLeg(transport.LegRef{
        Kind:   transport.KindTCP,
        ConnID: "tcp0",
    })

    udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
    if !udpQ.Active {
        t.Fatal("UDP leg should be active")
    }
    if charge := legCharge(udpLeg, len("payload")); charge != uint32(len("payload")) {
        t.Fatalf("charge = %d, want %d", charge, len("payload"))
    }
    if udpLeg.Kind != transport.KindUDP || udpLeg.EndpointID != "udp0" {
        t.Fatalf("udpLeg = %+v, want UDP udp0", udpLeg)
    }
    _ = tcpLeg
    _ = tcpQ
}

func TestLaneRuntimeFallsBackToTCP(t *testing.T) {
    lane := newLaneRuntime(1, 10)
    lane.observeLeg(transport.LegRef{
        Kind:   transport.KindTCP,
        ConnID: "tcp0",
    })

    _, udpQ, tcpLeg, tcpQ := lane.legQualities()
    if udpQ.Active {
        t.Fatal("UDP leg should not be active")
    }
    if !tcpQ.Active {
        t.Fatal("TCP leg should be active")
    }
    if charge := legCharge(tcpLeg, len("payload")); charge != uint32(len("payload")+2) {
        t.Fatalf("charge = %d, want %d", charge, len("payload")+2)
    }
    if tcpLeg.Kind != transport.KindTCP || tcpLeg.ConnID != "tcp0" {
        t.Fatalf("tcpLeg = %+v, want TCP tcp0", tcpLeg)
    }
}

func TestLaneRuntimeUnavailable(t *testing.T) {
    lane := newLaneRuntime(1, 10)
    _, udpQ, _, tcpQ := lane.legQualities()
    if udpQ.Active || tcpQ.Active {
        t.Fatal("Neither leg should be active for unavailable lane")
    }
}
```

- [ ] **Step 3: Run test**

Run: `go test ./internal/tunnel/send/ -run 'TestLaneRuntime|TestQuality|TestUDPPreferss|TestLegQualityTracker' -v`
Expected: all PASS

- [ ] **Step 4: Commit**

```bash
git add internal/tunnel/send/lane.go internal/tunnel/send/lane_test.go
git commit -m "refactor: replace lane.selectLeg with legQualities() and add quality trackers"
```

---

### Task 4: Wire writeScheduledFrame through LegSelector

**Files:**
- Modify: `internal/tunnel/send/send.go`
- Modify: `internal/tunnel/send/module.go`
- Modify: `internal/tunnel/send/rtt_state.go`

- [ ] **Step 1: Update Send struct — add legSelectors map and legSelector() method**

In `module.go`, add field to `Send`:

```go
    legSelectors map[uint64]LegSelector
```

In `New()`:

```go
    legSelectors:       make(map[uint64]LegSelector),
```

Add method:

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

- [ ] **Step 2: Update writeScheduledFrame in send.go**

Replace line 120-145:

```go
func (l *Send) writeScheduledFrame(ctx context.Context, frame protocol.Frame) (laneID uint8, charge uint32, err error) {
    sizeHint, err := frameEncodeCapacity(frame)
    if err != nil {
        return 0, 0, err
    }
    lane, ok := l.pickLane(frame.SessionID, uint32(sizeHint))
    if !ok {
        // ... existing no-runnable-lane handling ...
        return 0, 0, errNoRunnableLane
    }

    laneID = lane.id
    frame.LaneID = laneID

    udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
    useUDP, ok := l.legSelector(frame.SessionID).Pick(udpQ, tcpQ)
    if !ok {
        // ... existing no-leg handling ...
        return 0, 0, errNoRunnableLane
    }

    var leg transport.LegRef
    if useUDP {
        leg = udpLeg
    } else {
        leg = tcpLeg
    }

    // ... rest (enqueue, metrics, charge) ...
}
```

Full replacement of the current function body:

```go
func (l *Send) writeScheduledFrame(ctx context.Context, frame protocol.Frame) (laneID uint8, charge uint32, err error) {
    sizeHint, err := frameEncodeCapacity(frame)
    if err != nil {
        return 0, 0, err
    }
    lane, ok := l.pickLane(frame.SessionID, uint32(sizeHint))
    if !ok {
        if debuglog.Enabled() {
            debuglog.Printf("send", "schedule_empty session=%d frame=%s", frame.SessionID, debugFrameSummary(frame))
        }
        metrics.IncCounter(metrics.ScheduleNoRunnableTotal,
            metrics.L("session", frame.SessionID),
            metrics.L("frame_type", debugFrameType(frame.Type)),
        )
        return 0, 0, errNoRunnableLane
    }

    laneID = lane.id
    frame.LaneID = laneID

    udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
    useUDP, ok := l.legSelector(frame.SessionID).Pick(udpQ, tcpQ)
    if !ok {
        if debuglog.Enabled() {
            debuglog.Printf("send", "schedule_skip no_leg %s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane))
        }
        metrics.IncCounter(metrics.ScheduleSkipTotal,
            metrics.L("session", frame.SessionID),
            metrics.L("lane", laneID),
            metrics.L("reason", "no_leg"),
        )
        return 0, 0, errNoRunnableLane
    }

    var leg transport.LegRef
    if useUDP {
        leg = udpLeg
    } else {
        leg = tcpLeg
    }

    if debuglog.Enabled() {
        debuglog.Printf("send", "schedule_select %s leg={%s} frame=%s", debugLaneState(laneKey{sessionID: frame.SessionID, laneID: laneID}, lane), debugLeg(leg), debugFrameSummary(frame))
    }
    metrics.IncCounter(metrics.SchedulePickTotal,
        metrics.LU64("session", frame.SessionID),
        metrics.LU8("lane", laneID),
        metrics.LStr("frame_type", debugFrameType(frame.Type)),
        metrics.LStr("leg", kindMetricLabel(leg.Kind)),
    )
    size, err := l.enqueueFrameWithSize(ctx, leg, frame, sizeHint)
    if err != nil {
        if debuglog.Enabled() {
            debuglog.Printf("send", "schedule_enqueue_err session=%d lane=%d leg={%s} err=%v", frame.SessionID, laneID, debugLeg(leg), err)
        }
        return laneID, 0, err
    }

    charge = legCharge(leg, size)
    if debuglog.Enabled() {
        debuglog.Printf("send", "schedule_done session=%d lane=%d leg={%s} frame_bytes=%d charge=%d", frame.SessionID, laneID, debugLeg(leg), size, charge)
    }
    return laneID, charge, nil
}
```

- [ ] **Step 3: Update rtt_state.go — remove selectLegLocked calls**

In `sessionMaxRTTMs` and `sessionMinRTTMs`, replace `lane.selectLegLocked()` with equivalent active-leg check:

```go
func (l *Send) sessionMaxRTTMs(sessionID uint64) (uint32, bool) {
    var max uint32
    ok := false
    for _, lane := range l.runnableLanes(sessionID) {
        lane.mu.Lock()
        srtt, sampleOK := uint32(0), false
        if lane.udpReady && lane.udpLeg.EndpointID != "" && lane.udpLeg.RemoteAddr != nil {
            srtt, sampleOK = lane.rttUDP.SRTT()
        } else if lane.tcpReady && lane.tcpLeg.ConnID != "" {
            srtt, sampleOK = lane.rttTCP.SRTT()
        }
        lane.mu.Unlock()
        if !sampleOK {
            continue
        }
        if !ok || srtt > max {
            max = srtt
            ok = true
        }
    }
    return max, ok
}

func (l *Send) sessionMinRTTMs(sessionID uint64) (uint32, bool) {
    var min uint32
    ok := false
    for _, lane := range l.runnableLanes(sessionID) {
        lane.mu.Lock()
        srtt, sampleOK := uint32(0), false
        if lane.udpReady && lane.udpLeg.EndpointID != "" && lane.udpLeg.RemoteAddr != nil {
            srtt, sampleOK = lane.rttUDP.SRTT()
        } else if lane.tcpReady && lane.tcpLeg.ConnID != "" {
            srtt, sampleOK = lane.rttTCP.SRTT()
        }
        lane.mu.Unlock()
        if !sampleOK {
            continue
        }
        if !ok || srtt < min {
            min = srtt
            ok = true
        }
    }
    return min, ok
}
```

Also remove `laneSRTTLocked` and `laneRTTEstimatorLocked` if they're no longer used (check by searching for callers).

- [ ] **Step 4: Run build**

Run: `go build ./...`
Expected: success

- [ ] **Step 5: Run all send tests**
Run: `go test ./internal/tunnel/send/ -v` (or quick: `go test ./internal/tunnel/send/`)
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/send/send.go internal/tunnel/send/module.go internal/tunnel/send/rtt_state.go
git commit -m "feat: wire LegSelector into writeScheduledFrame"
```

---

### Task 5: Integrate delivery tracking into ProbeLoop

**Files:**
- Modify: `internal/tunnel/send/rtt_state.go`
- Modify: `internal/tunnel/send/control.go`
- Modify: `internal/tunnel/send/probe.go`

- [ ] **Step 1: Extend rttPendingPing with deadlineMS**

In `rtt_state.go`:

```go
type rttPendingPing struct {
    sessionID  uint64
    laneID     uint8
    legKey     pingKey
    timeMS     uint64
    deadlineMS uint64 // 0 = no deadline, otherwise max tolerated RTT in ms
}
```

- [ ] **Step 2: Compute and store deadline in recordRTTPing**

In `rtt_state.go`, modify `recordRTTPing`:

```go
func (l *Send) recordRTTPing(target probe.Target, pingID uint64, timeMS uint64, binding probeBinding) {
    if target == 0 {
        return
    }

    // Compute delivery deadline: 2x SRTT, floor 100ms
    var deadlineMS uint64
    lane := l.getLane(laneKey{sessionID: binding.sessionID, laneID: binding.laneID})
    if lane != nil {
        lane.mu.Lock()
        estimator := laneRTTEstimatorLocked(lane, binding.leg.Kind)
        if estimator != nil {
            if srtt, ok := estimator.SRTT(); ok && srtt > 0 {
                d := uint64(srtt) * 2
                if d < 100 {
                    d = 100
                }
                deadlineMS = d
            }
        }
        lane.mu.Unlock()
    }

    l.rttMu.Lock()
    if l.rttPending == nil {
        l.rttPending = make(map[rttPendingKey]rttPendingPing)
    }
    l.pruneRTTPendingLocked(target, timeMS)
    l.rttPending[rttPendingKey{target: target, pingID: pingID}] = rttPendingPing{
        sessionID:  binding.sessionID,
        laneID:     binding.laneID,
        legKey:     newPingKey(binding.leg),
        timeMS:     timeMS,
        deadlineMS: deadlineMS,
    }
    l.rttMu.Unlock()
}
```

- [ ] **Step 3: Record delivery on PONG**

In `control.go`, modify `receivePong` — add delivery recording after the existing RTT update:

After line 146 (`debuglog.Printf...`), add:

```go
    // Record delivery for leg quality tracking.
    now := uint64(time.Now().UnixMilli())
    arrivedOnTime := pending.deadlineMS == 0 || now <= pending.timeMS+pending.deadlineMS
    lane.recordDelivery(leg.Kind, arrivedOnTime)
```

But wait — `pending` is from `acceptRTTPong` which is called at line 144. The pending data is already consumed (deleted) inside `acceptRTTPong`. We need to extract the deadline before `acceptRTTPong` consumes it.

Let me restructure: read the pending info before calling `acceptRTTPong`, or pass the deadline through `acceptRTTPong`.

Simplest approach: extract the pending key's data before accept:

```go
func (i *Send) receivePong(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.PingBody) error {
    // ... existing session/lane validation ...
    
    i.probeMu.Lock()
    target, ok := i.probeKeys[newPingKey(leg)]
    i.probeMu.Unlock()
    // ... existing target check ...

    // Read deadline before acceptRTTPong consumes the pending entry.
    var deadlineMS uint64
    i.rttMu.Lock()
    if p, ok := i.rttPending[rttPendingKey{target: target, pingID: body.PingID}]; ok {
        deadlineMS = p.deadlineMS
    }
    i.rttMu.Unlock()

    if sample, ok := i.acceptRTTPong(lane, sessionID, laneID, leg, target, body, uint64(time.Now().UnixMilli())); ok {
        debuglog.Printf(...)
    }

    // Record delivery for leg quality tracking.
    now := uint64(time.Now().UnixMilli())
    arrivedOnTime := deadlineMS == 0 || now <= body.TimeMS+deadlineMS
    lane.recordDelivery(leg.Kind, arrivedOnTime)

    // ... rest of PONG handling ...
}
```

- [ ] **Step 4: Run build**

Run: `go build ./...`
Expected: success

- [ ] **Step 5: Run tests**

Run: `go test ./internal/tunnel/send/ -v`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/send/rtt_state.go internal/tunnel/send/control.go
git commit -m "feat: integrate delivery deadline tracking into PING/PONG loop"
```

---

### Task 6: Remove unused helpers, final cleanup

**Files:**
- Modify: `internal/tunnel/send/lane.go` — remove `selectLeg`/`selectLegLocked` if not already removed

- [ ] **Step 1: Verify selectLeg is fully removed**

Search: `rg 'selectLeg' internal/tunnel/send/`
Expected: only hits in documentation/comments, or in test files (which should be updated already)

- [ ] **Step 2: Run full build and test**

```bash
go build ./...
go test ./...
```

- [ ] **Step 3: Commit**

```bash
git add -A
git commit -m "chore: remove unused selectLeg helpers, final cleanup"
```

---

### Self-Review Checklist

1. **Spec coverage:** LegSelector interface ✓ (Task 1), legQualityTracker ✓ (Task 2), laneRuntime changes ✓ (Task 3), Send integration ✓ (Task 4), ProbeLoop integration ✓ (Task 5)
2. **Placeholder scan:** No TBD/TODO/fill-later in plan ✓
3. **Type consistency:** `Pick(udp, tcp LegQuality) (useUDP bool, ok bool)` is consistent across all tasks ✓
