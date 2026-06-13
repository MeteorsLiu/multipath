# DRR Scheduler Design

Date: 2026-06-11
Status: Draft for review
Branch: v2

## 1. Scope

This spec defines the DRR schedule strategy used by Send for DATA lane
selection.

DRR is implemented behind the existing schedule strategy interface:

```go
type Lane interface {
    comparable
    Weight() uint32
}

type Strategy[L Lane] interface {
    Pick(lanes []L, cost uint32) (L, bool)
}
```

This spec does not change the scheduler interface.

## 2. Goals

- Keep long-term lane fairness according to lane weight.
- Prefer short bursts on the same lane so per-lane FEC can form full groups.
- Keep scheduler logic independent of protocol frames, FEC windows, transport
  refs, QoS state, and lane runtime internals.
- Make DRR the default schedule strategy after the send/lane refactor.

## 3. Non-Goals

- Do not add methods to `schedule.Strategy`.
- Do not make the scheduler know protocol frame types.
- Do not make the scheduler choose UDP or TCP.
- Do not make the scheduler send packets.
- Do not make the scheduler own FEC windows.
- Do not define QoS policy in this spec.
- Do not define LINK_STATUS behavior in this spec.

## 4. Package

Add:

```text
internal/schedule/drr
```

The package exports a strategy constructor equivalent in role to the current
CFS constructor. This spec does not require a new method on
`schedule.Strategy`.

Send must construct the default DRR strategy with `baseQuantum = 4 * MTU`.

## 5. State

The strategy owns all DRR fairness state.

Per lane:

```text
lane
deficit
lastSeen
```

Strategy:

```text
items map[L]*item
order []L
cursor
round
baseQuantum
```

`baseQuantum` is measured in bytes.

## 6. Quantum

The target burst quantum is:

```text
baseQuantum = 4 * MTU
```

The purpose is to let one selected lane send approximately one full 4-DATA FEC
group before another equal-weight lane becomes preferred.

For weighted lanes, quantum is scaled by lane weight:

```text
quantumBytes = baseQuantum * lane.Weight()
```

Weight `0` means the lane is not schedulable.

DRR adds quantum to deficit when the current deficit cannot cover the packet
cost:

```text
if deficit < cost:
  deficit += quantumBytes
```

DRR does not implement bandwidth refill. Per-lane bandwidth control is handled
before DRR by Send/lane runtime.

## 7. Pick Semantics

Input:

```text
lanes: current runnable lane candidates supplied by Send
cost: estimated DATA frame send cost in bytes
```

Output:

```text
selected lane, true
zero lane, false
```

Rules:

```text
1. If lanes is empty, return false.
2. Ignore lanes whose Weight() is 0.
3. Synchronize internal item state with the current lane set.
4. Start scanning at cursor.
5. For each candidate lane:
     if deficit < cost:
       deficit += baseQuantum * Weight()
     if deficit >= cost:
       deficit -= cost
       select this lane
       if remaining deficit >= cost:
         keep cursor on this lane
       else:
         move cursor to the next candidate
       return true
6. If no candidate can cover cost after adding one weighted quantum, return
   false.
7. If no candidate has positive weight, return false.
```

The implementation must not spin. If candidates cannot cover `cost` after
normal DRR quantum addition, `Pick` returns false and Send applies its existing
no-runnable-lane behavior for that packet.

## 8. Cursor Behavior

After selecting a lane, cursor behavior is based on the same `cost` passed to
that `Pick` call:

```text
remaining deficit >= cost:
  keep cursor on the selected lane

remaining deficit < cost:
  move cursor to the next candidate
```

This preserves the desired short-burst behavior:

```text
lane1, lane1, lane1, lane1,
lane2, lane2, lane2, lane2,
...
```

for equal weights and packets near MTU size when `baseQuantum = 4 * MTU`.

If the current lane cannot cover the current cost after adding one weighted
quantum, cursor scanning continues to other runnable lanes. If no lane can
cover the cost, `Pick` returns false.

## 9. Cost Accounting

`Pick` subtracts exactly the `cost` passed by Send from the selected lane's
deficit.

Send is responsible for estimating cost before calling `Pick`.

The scheduler does not inspect frame bodies and does not compute protocol
encoding overhead itself.

## 10. Dynamic Lane Set

Send passes the current runnable lane set on every `Pick`.

DRR handles dynamic lane membership as follows:

```text
new lane:
  create item with deficit = 0

missing lane:
  do not consider it for selection while absent

returned lane:
  treat it as the same lane value if the retained item still exists
```

The cleanup policy is internal to the strategy. It must not require new methods
on `schedule.Strategy`.

## 11. Relationship To FEC

DRR does not own FEC state.

The only FEC-related behavior DRR provides is packet selection shape: by using
`baseQuantum = 4 * MTU`, equal-weight lanes tend to receive enough consecutive
DATA packets to fill one lane-local FEC group.

FEC group construction remains in Send/lane runtime.

## 12. Relationship To REPAIR

REPAIR frames do not call `Pick`.

REPAIR is emitted by the lane that owns the FEC group, as specified by the
send/lane/recvhandler refactor spec.

Therefore DRR fairness accounting applies to DATA lane selection only.

## 13. Relationship To QoS

DRR does not implement QoS detection or switching.

DRR does not know:

```text
limited/backlogged state
LINK_STATUS
primary/shadow transport role
UDP/TCP transport quality
```

Any QoS behavior is outside this scheduler spec.

## 14. Example

Assume:

```text
lanes: A, B
weights: A=1, B=1
baseQuantum: 4 * MTU
cost: approximately 1 * MTU
```

Expected selection shape:

```text
A A A A B B B B A A A A B B B B ...
```

Long-term bytes are equal across A and B, while short-term scheduling gives
each lane enough consecutive DATA to form FEC groups.

With weights:

```text
A=2, B=1
```

Expected long-term byte ratio:

```text
A:B = 2:1
```

## 15. Testing Requirements

| Area | Required check |
|---|---|
| empty input | Empty lane list returns false |
| zero weight | Weight 0 lanes are ignored |
| short burst | Equal lanes and MTU-sized cost produce approximately 4 picks per lane turn |
| weighted fairness | Long run byte ratio follows lane weights |
| variable cost | Deficit subtracts the passed cost, not packet count |
| quantum add | Deficit increases by weighted DRR quantum |
| large cost | Cost larger than current deficit plus one quantum returns false |
| dynamic lanes | New and removed runnable lanes do not corrupt cursor or deficit state |
| isolation | DRR package imports schedule only, not protocol, transport, FEC, send, or recv |
