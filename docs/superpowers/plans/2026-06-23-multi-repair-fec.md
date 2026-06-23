# Multi-Repair FEC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the accepted multi-repair packet FEC spec: adaptive lane-local `4+N` repair emission, multi-shard receive recovery, and LINK_STATUS repair-count feedback through lane-internal `setFEC`.

**Architecture:** Keep the public module boundaries unchanged. `internal/fec` remains shard-level; `send` owns lane-local transmit repair count and REPAIR emission; `recv` owns receive windows, recovery, byte accounting, and repair-count recommendation; runtime only converts between Recv feedback and LINK_STATUS frames. Do not add an exported FEC setter on Send, LaneManager, or Session.

**Tech Stack:** Go, `github.com/klauspost/reedsolomon`, existing v2 `send`, `recv`, `runtime`, `protocol`, `packetbuf`, and current Superpowers spec `docs/superpowers/specs/2026-06-23-multi-repair-fec-design.md`.

---

## Scope Check

This plan implements one spec. It touches multiple packages, but the changes are one protocol/data-flow increment: multi-repair FEC and its lane-local feedback path. Do not add retransmission, new FEC frames, public Lane modules, public protocol helpers, or compatibility profiles.

## Subagent Execution Rule

All implementer and reviewer subagents for this plan must follow the minimum-implementation rule:

- Do not add defensive validation, compatibility paths, fallback behavior, state, fields, abstractions, or tests for cases not required by the current approved spec.
- Do not reject inputs based on guessed future failure modes. Validate only the shape constraints required by the protocol or local API contract.
- Do not precompute or pre-prove behavior for hypothetical future packet-loss patterns. Execute the current operation and return the existing error when it fails.
- When a reviewer flags a possible edge case, first ask whether the approved requirement actually needs a new check. Do not treat extra checks as automatically safer.

## File Structure

- `internal/fec/fec.go`: shard-level codec, now `keys []uint16` and `repairShards` in `1..4`.
- `internal/fec/fec_test.go`: codec behavior for `4+1`, `4+2`, `4+3`, mixed-size shards, and invalid keys.
- `internal/fec/fec_bench_test.go`: benchmark call sites updated to `keys []uint16`.
- `internal/protocol/body.go`: LINK_STATUS nibble validation accepts state values `0..7`.
- `internal/protocol/body_test.go`: protocol tests for repair-count bits and invalid nibbles.
- `internal/tunnel/v2/runtime/qos.go`: encode `recv.QoSStatus.RepairCount` into both LINK_STATUS nibbles.
- `internal/tunnel/v2/runtime/recvhandler.go`: decode LINK_STATUS nibbles, require matching repair counts, pass repair count into `send.QoSInput`.
- `internal/tunnel/v2/runtime/*_test.go`: writer and handler tests for repair-count bits.
- `internal/tunnel/v2/send/lanemanager.go`: extend `QoSInput.OnQoSStatus` with `repairCount`.
- `internal/tunnel/v2/send/lane.go`: lane stores current FEC repair count and has unexported `setFEC`.
- `internal/tunnel/v2/send/send.go`: send `repairCount` REPAIR frames per emitted group.
- `internal/tunnel/v2/send/*_test.go`: lane feedback and multi-repair emission tests.
- `internal/tunnel/v2/recv/rx_group_window.go`: already group-keyed; keep it FEC-only.
- `internal/tunnel/v2/recv/recv.go`: use multi-repair codecs, emit multiple recovered packets, track minimal group byte facts, and attach repair count to QoS feedback.
- `internal/tunnel/v2/recv/qos_estimator.go`: stop using FEC health as a direct QoS limited/clear source; keep rate-based decisions.
- `internal/tunnel/v2/recv/*_test.go`: receive multi-missing recovery, byte accounting, adaptive repair count, and no duplicate DATA accounting.
- `docs/protocol.md`: update LINK_STATUS nibble layout and lane-local FEC wording.

---

### Task 1: FEC Codec Supports Multiple Repair Shards

**Files:**
- Modify: `internal/fec/fec.go`
- Modify: `internal/fec/fec_test.go`
- Modify: `internal/fec/fec_bench_test.go`

- [ ] **Step 1: Write failing codec tests**

Add these tests to `internal/fec/fec_test.go`:

```go
func TestCodecReconstructsTwoMissingShardsWithTwoRepairs(t *testing.T) {
	codec, err := NewCodec(4, 2)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	source := [][]byte{
		[]byte("aaaa"),
		[]byte("bbb"),
		[]byte("cc"),
		[]byte("d"),
		nil,
		nil,
	}
	keys := []uint16{7, 8}
	if err := codec.Encode(source, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	repairs := [][]byte{
		append([]byte(nil), source[4]...),
		append([]byte(nil), source[5]...),
	}
	recovered := [][]byte{
		append([]byte(nil), source[0]...),
		nil,
		append([]byte(nil), source[2]...),
		nil,
		repairs[0],
		repairs[1],
	}
	if err := codec.Reconstruct(recovered, keys); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if !bytes.Equal(recovered[1], []byte{'b', 'b', 'b', 0}) {
		t.Fatalf("recovered[1] = %v, want padded bbb", recovered[1])
	}
	if !bytes.Equal(recovered[3], []byte{'d', 0, 0, 0}) {
		t.Fatalf("recovered[3] = %v, want padded d", recovered[3])
	}
}

func TestCodecReconstructsThreeMissingShardsWithThreeRepairs(t *testing.T) {
	codec, err := NewCodec(4, 3)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	source := [][]byte{
		[]byte("abcd"),
		[]byte("ef"),
		[]byte("ghij"),
		[]byte("k"),
		nil,
		nil,
		nil,
	}
	keys := []uint16{11, 12, 13}
	if err := codec.Encode(source, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	recovered := [][]byte{
		nil,
		append([]byte(nil), source[1]...),
		nil,
		nil,
		append([]byte(nil), source[4]...),
		append([]byte(nil), source[5]...),
		append([]byte(nil), source[6]...),
	}
	if err := codec.Reconstruct(recovered, keys); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if !bytes.Equal(recovered[0], []byte("abcd")) {
		t.Fatalf("recovered[0] = %q, want abcd", recovered[0])
	}
	if !bytes.Equal(recovered[2], []byte("ghij")) {
		t.Fatalf("recovered[2] = %q, want ghij", recovered[2])
	}
	if !bytes.Equal(recovered[3], []byte{'k', 0, 0, 0}) {
		t.Fatalf("recovered[3] = %v, want padded k", recovered[3])
	}
}

func TestCodecRejectsWrongKeyCount(t *testing.T) {
	codec, err := NewCodec(4, 2)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), nil, nil}
	if err := codec.Encode(shards, []uint16{1}); !errors.Is(err, ErrInvalidShardConfig) {
		t.Fatalf("Encode err = %v, want ErrInvalidShardConfig", err)
	}
	if err := codec.Reconstruct(shards, []uint16{1}); !errors.Is(err, ErrInvalidShardConfig) {
		t.Fatalf("Reconstruct err = %v, want ErrInvalidShardConfig", err)
	}
}
```

Update existing codec tests and benchmarks so calls use slices:

```go
codec.Encode(shards, []uint16{7})
codec.Reconstruct(recovered, []uint16{7})
```

Change `TestNewCodecRejectsUnsupportedRepairCount` to reject zero repairs:

```go
func TestNewCodecRejectsInvalidRepairCount(t *testing.T) {
	if _, err := NewCodec(4, 0); !errors.Is(err, ErrInvalidShardConfig) {
		t.Fatalf("NewCodec err = %v, want ErrInvalidShardConfig", err)
	}
}
```

- [ ] **Step 2: Run focused tests and confirm failure**

Run:

```bash
go test ./internal/fec -run 'TestCodec|TestNewCodec' -count=1
```

Expected: compile failures because `Encode` and `Reconstruct` still accept a single `uint16`, and `NewCodec(4, 2)` is still rejected.

- [ ] **Step 3: Implement minimal codec changes**

In `internal/fec/fec.go`, change the public methods and validation:

```go
func NewCodec(dataShards, repairShards int) (*Codec, error) {
	if dataShards <= 0 || repairShards <= 0 || repairShards > 4 {
		debuglog.Printf("fec", "new_codec_err data_shards=%d repair_shards=%d err=%v", dataShards, repairShards, ErrInvalidShardConfig)
		return nil, ErrInvalidShardConfig
	}
	debuglog.Printf("fec", "new_codec data_shards=%d repair_shards=%d", dataShards, repairShards)
	return &Codec{dataShards: dataShards, repairShards: repairShards}, nil
}

func (c *Codec) validate(shards [][]byte, keys []uint16) error {
	if c == nil || c.dataShards <= 0 || c.repairShards <= 0 || c.repairShards > 4 {
		return ErrInvalidShardConfig
	}
	if len(shards) != c.dataShards+c.repairShards || len(keys) != c.repairShards {
		return ErrInvalidShardConfig
	}
	return nil
}
```

Add these helpers to `internal/fec/fec.go`:

```go
func (c *Codec) encoder(keys []uint16) (reedsolomon.Encoder, error) {
	matrix := make([][]byte, c.repairShards)
	for i, key := range keys {
		row := make([]byte, c.dataShards)
		fillCodingCoefficients(key, row)
		matrix[i] = row
	}
	return reedsolomon.New(
		c.dataShards,
		c.repairShards,
		reedsolomon.WithCustomMatrix(matrix),
		reedsolomon.WithMaxGoroutines(1),
	)
}

func maxShardLen(shards [][]byte) int {
	size := 0
	for _, shard := range shards {
		if len(shard) > size {
			size = len(shard)
		}
	}
	return size
}

func paddedShard(shard []byte, size int) []byte {
	if len(shard) == size {
		return shard
	}
	out := make([]byte, size)
	copy(out, shard)
	return out
}
```

Replace `Encode` with:

```go
func (c *Codec) Encode(shards [][]byte, keys []uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "encode_start keys=%v data_shards=%d repair_shards=%d lens=%v", keys, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards, keys); err != nil {
		debuglog.Printf("fec", "encode_validate_err keys=%v err=%v", keys, err)
		return err
	}
	repairLen := maxShardLen(shards[:c.dataShards])
	if repairLen == 0 {
		debuglog.Printf("fec", "encode_err keys=%v err=%v reason=empty_repair", keys, ErrInvalidShardConfig)
		return ErrInvalidShardConfig
	}
	work := make([][]byte, c.dataShards+c.repairShards)
	for i := 0; i < c.dataShards; i++ {
		if shards[i] == nil {
			debuglog.Printf("fec", "encode_err keys=%v shard=%d err=%v reason=nil_data_shard", keys, i, ErrInvalidShardConfig)
			return ErrInvalidShardConfig
		}
		work[i] = paddedShard(shards[i], repairLen)
	}
	for i := 0; i < c.repairShards; i++ {
		repair := shards[c.dataShards+i]
		if cap(repair) < repairLen {
			repair = make([]byte, repairLen)
		} else {
			repair = repair[:repairLen]
			clear(repair)
		}
		work[c.dataShards+i] = repair
	}
	encoder, err := c.encoder(keys)
	if err != nil {
		return err
	}
	if err := encoder.Encode(work); err != nil {
		return err
	}
	for i := 0; i < c.repairShards; i++ {
		shards[c.dataShards+i] = work[c.dataShards+i]
	}
	if debuglog.Enabled() {
		debuglog.Printf("fec", "encode_done keys=%v repair_len=%d", keys, repairLen)
	}
	return nil
}
```

Replace `Reconstruct` with:

```go
func (c *Codec) Reconstruct(shards [][]byte, keys []uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "reconstruct_start keys=%v data_shards=%d repair_shards=%d lens=%v", keys, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards, keys); err != nil {
		debuglog.Printf("fec", "reconstruct_validate_err keys=%v err=%v", keys, err)
		return err
	}
	shardLen := maxShardLen(shards)
	if shardLen == 0 {
		return ErrUnrecoverable
	}
	missingData := 0
	work := make([][]byte, c.dataShards+c.repairShards)
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) == 0 {
			missingData++
			continue
		}
		if len(shards[i]) > shardLen {
			return ErrUnrecoverable
		}
		work[i] = paddedShard(shards[i], shardLen)
	}
	for i := 0; i < c.repairShards; i++ {
		shard := shards[c.dataShards+i]
		if len(shard) == 0 {
			continue
		}
		if len(shard) != shardLen {
			return ErrUnrecoverable
		}
		work[c.dataShards+i] = shard
	}
	if missingData == 0 || missingData > c.repairShards {
		return ErrUnrecoverable
	}
	encoder, err := c.encoder(keys)
	if err != nil {
		return err
	}
	if err := encoder.ReconstructData(work); err != nil {
		debuglog.Printf("fec", "reconstruct_err keys=%v missing_count=%d err=%v", keys, missingData, err)
		return ErrUnrecoverable
	}
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) == 0 {
			shards[i] = work[i]
		}
	}
	if debuglog.Enabled() {
		debuglog.Printf("fec", "reconstruct_done keys=%v recovered_len=%d", keys, shardLen)
	}
	return nil
}
```

Remove the direct low-level one-repair reconstruction logic from the old methods. Keep `fillCodingCoefficients` and TinyMT unchanged.

- [ ] **Step 4: Run focused tests**

Run:

```bash
go test ./internal/fec -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/fec/fec.go internal/fec/fec_test.go internal/fec/fec_bench_test.go
git commit -m "Support multi-repair FEC codec"
```

---

### Task 2: LINK_STATUS Nibble Validation and Runtime Encoding

**Files:**
- Modify: `internal/protocol/body.go`
- Modify: `internal/protocol/body_test.go`
- Modify: `internal/tunnel/v2/recv/recv.go`
- Modify: `internal/tunnel/v2/runtime/qos.go`
- Modify: `internal/tunnel/v2/runtime/qos_test.go`

- [ ] **Step 1: Write failing protocol/runtime tests**

In `internal/protocol/body_test.go`, update `TestLinkStatusRoundTrip` to use repair bits:

```go
func TestLinkStatusRoundTrip(t *testing.T) {
	frame := Frame{
		Type:      TypeLinkStatus,
		SessionID: 11,
		LaneID:    1,
		Body: LinkStatusBody{
			Status:          0x54,
			UDPDeliveredBps: 2_000_000,
			TCPDeliveredBps: 8_000_000,
		},
	}
	encoded, err := Encode(frame, nil)
	if err != nil {
		t.Fatalf("Encode LINK_STATUS failed: %v", err)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode LINK_STATUS failed: %v", err)
	}
	assertFrameEqual(t, got, frame)
}
```

Replace `TestLinkStatusRejectsInvalidBody` with:

```go
func TestLinkStatusRejectsInvalidBody(t *testing.T) {
	tests := []LinkStatusBody{
		{Status: 0x80, UDPDeliveredBps: 1},
		{Status: 0x08, TCPDeliveredBps: 1},
		{Status: 0xf0, UDPDeliveredBps: 1},
		{Status: 0x0f, TCPDeliveredBps: 1},
	}
	for _, body := range tests {
		_, err := Encode(Frame{Type: TypeLinkStatus, SessionID: 1, LaneID: 1, Body: body}, nil)
		if !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("Encode(%+v) err = %v, want ErrInvalidFrame", body, err)
		}
	}
}
```

In `internal/tunnel/v2/runtime/qos_test.go`, update the writer status and assertion:

```go
err := writer.Write(ctx, recv.QoSStatus{
	SessionID:       sessionID,
	LaneID:          1,
	UDPLimited:      true,
	RepairCount:     3,
	UDPDeliveredBps: 2_000_000,
	TCPDeliveredBps: 8_000_000,
})
```

Expected status byte:

```go
wantStatus := uint8(0x54)
if body.Status != wantStatus || body.UDPDeliveredBps != 2_000_000 || body.TCPDeliveredBps != 8_000_000 {
	t.Fatalf("body = %+v, want status %#02x", body, wantStatus)
}
```

- [ ] **Step 2: Run focused tests and confirm failure**

Run:

```bash
go test ./internal/protocol ./internal/tunnel/v2/runtime -run 'TestLinkStatus|TestQoSWriterWritesLinkStatusFrame' -count=1
```

Expected: protocol rejects state `0x54`, and `recv.QoSStatus` does not yet have `RepairCount`.

- [ ] **Step 3: Implement protocol validation and writer encoding**

In `internal/protocol/body.go`, change only `validLinkStatusState`:

```go
func validLinkStatusState(state uint8) bool {
	return state <= 7
}
```

In `internal/tunnel/v2/recv/recv.go`, extend public feedback:

```go
type QoSStatus struct {
	SessionID       uint64
	LaneID          uint8
	UDPLimited      bool
	TCPLimited      bool
	RepairCount     uint8
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
}
```

In `internal/tunnel/v2/runtime/qos.go`, replace `linkStatusByte` with:

```go
func linkStatusByte(udpLimited, tcpLimited bool, repairCount uint8) uint8 {
	if repairCount < 1 || repairCount > 4 {
		repairCount = 1
	}
	repairCode := (repairCount - 1) << 1
	udpState := repairCode
	tcpState := repairCode
	if udpLimited {
		udpState |= protocol.LinkStatusStateLimited
	}
	if tcpLimited {
		tcpState |= protocol.LinkStatusStateLimited
	}
	return (udpState << 4) | tcpState
}
```

Update `Write` to call:

```go
linkStatus := linkStatusByte(status.UDPLimited, status.TCPLimited, status.RepairCount)
```

- [ ] **Step 4: Run focused tests**

Run:

```bash
go test ./internal/protocol ./internal/tunnel/v2/runtime -run 'TestLinkStatus|TestQoSWriterWritesLinkStatusFrame' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/protocol/body.go internal/protocol/body_test.go internal/tunnel/v2/recv/recv.go internal/tunnel/v2/runtime/qos.go internal/tunnel/v2/runtime/qos_test.go
git commit -m "Encode repair count in LINK_STATUS"
```

---

### Task 3: Runtime Feedback Enters Lane-Internal setFEC

**Files:**
- Modify: `internal/tunnel/v2/send/lanemanager.go`
- Modify: `internal/tunnel/v2/send/lane.go`
- Modify: `internal/tunnel/v2/send/lane_role_test.go`
- Modify: `internal/tunnel/v2/runtime/recvhandler.go`
- Modify: `internal/tunnel/v2/runtime/recvhandler_test.go`

- [ ] **Step 1: Write failing lane/runtime tests**

Add to `internal/tunnel/v2/send/lane_role_test.go`:

```go
func TestLaneQoSInputUpdatesFECRepairCount(t *testing.T) {
	l := newLaneRuntime(1, 100)
	if got := l.currentFECRepairCount(); got != 1 {
		t.Fatalf("initial repair count = %d, want 1", got)
	}
	laneQoSInput{lane: l}.OnQoSStatus(true, 2_000_000, false, 8_000_000, 3)
	if got := l.currentFECRepairCount(); got != 3 {
		t.Fatalf("repair count = %d, want 3", got)
	}
}

func TestLaneSetFECRejectsInvalidRepairCount(t *testing.T) {
	l := newLaneRuntime(1, 100)
	l.setFEC(4)
	l.setFEC(0)
	if got := l.currentFECRepairCount(); got != 4 {
		t.Fatalf("repair count after zero = %d, want 4", got)
	}
	l.setFEC(5)
	if got := l.currentFECRepairCount(); got != 4 {
		t.Fatalf("repair count after five = %d, want 4", got)
	}
}
```

In `internal/tunnel/v2/runtime/recvhandler_test.go`, add this package-level test helper near the other test helpers:

```go
type recordingQoSInput struct {
	udpLimited      bool
	tcpLimited      bool
	udpDeliveredBps uint32
	tcpDeliveredBps uint32
	repairCount     uint8
	called          bool
}

func (r *recordingQoSInput) OnQoSStatus(udpLimited bool, udpDeliveredBps uint32, tcpLimited bool, tcpDeliveredBps uint32, repairCount uint8) {
	r.udpLimited = udpLimited
	r.tcpLimited = tcpLimited
	r.udpDeliveredBps = udpDeliveredBps
	r.tcpDeliveredBps = tcpDeliveredBps
	r.repairCount = repairCount
	r.called = true
}
```

Inside the test, replace this existing nil check:

```go
qos := s.LaneManager().LookupQoS(send.LaneKey{SessionID: sessionID, LaneID: 1})
if qos == nil {
	t.Fatal("missing QoS input")
}
```

with:

```go
rec := &recordingQoSInput{}
s.LaneManager().RegisterQoS(send.LaneKey{SessionID: sessionID, LaneID: 1}, rec)
```

After `handler.OnQoS`, assert:

```go
if !rec.called {
	t.Fatal("QoS input was not called")
}
if !rec.udpLimited || rec.tcpLimited || rec.udpDeliveredBps != 2_000_000 || rec.tcpDeliveredBps != 8_000_000 || rec.repairCount != 3 {
	t.Fatalf("recorded QoS = %+v, want UDP limited and repair count 3", rec)
}
```

Set the frame status to repair count `3` with UDP limited:

```go
Status:          0x54,
```

- [ ] **Step 2: Run focused tests and confirm failure**

Run:

```bash
go test ./internal/tunnel/v2/send ./internal/tunnel/v2/runtime -run 'TestLaneQoSInputUpdatesFECRepairCount|TestLaneSetFECRejectsInvalidRepairCount|TestRecvHandlerOnQoSFeedsQoSInput' -count=1
```

Expected: compile failures because `OnQoSStatus` has no `repairCount`, lane has no FEC repair count helpers, and runtime does not decode repair count.

- [ ] **Step 3: Extend QoSInput and lane-internal FEC state**

In `internal/tunnel/v2/send/lanemanager.go`, change the interface:

```go
type QoSInput interface {
	OnQoSStatus(udpLimited bool, udpDeliveredBps uint32, tcpLimited bool, tcpDeliveredBps uint32, repairCount uint8)
}
```

In `internal/tunnel/v2/send/lane.go`, add to `laneRuntime`:

```go
fecRepairCount uint8
```

Set the default in `newLaneRuntime`:

```go
l := &laneRuntime{
	id:             id,
	txWindow:       newTxSLCWindow(maxFECSourceSpan),
	fecRepairCount: 1,
	leg:            newLeg(transport.KindUDP, &selector.QualitySelector{}),
}
```

Add unexported methods:

```go
func (l *laneRuntime) setFEC(repairCount uint8) {
	if l == nil || repairCount < 1 || repairCount > 4 {
		return
	}
	l.fecMu.Lock()
	l.fecRepairCount = repairCount
	l.fecMu.Unlock()
}

func (l *laneRuntime) currentFECRepairCount() uint8 {
	if l == nil {
		return 1
	}
	l.fecMu.Lock()
	defer l.fecMu.Unlock()
	if l.fecRepairCount == 0 {
		return 1
	}
	return l.fecRepairCount
}
```

Change `laneQoSInput.OnQoSStatus`:

```go
func (i laneQoSInput) OnQoSStatus(udpLimited bool, udpDeliveredBps uint32, tcpLimited bool, tcpDeliveredBps uint32, repairCount uint8) {
	if i.lane == nil {
		return
	}
	i.lane.leg.observeQoSStatus(udpLimited, udpDeliveredBps, tcpLimited, tcpDeliveredBps)
	i.lane.setFEC(repairCount)
	if debuglog.Enabled() {
		primary := i.lane.primaryTransport()
		shadow := i.lane.shadowTransport()
		debuglog.Printf("send/qos", "apply session=%d lane=%d primary={%s} shadow={%s} udp_limited=%t udp_delivered_bps=%d tcp_limited=%t tcp_delivered_bps=%d fec_repair_count=%d",
			i.sessionID, i.lane.id, debugLeg(primary), debugLeg(shadow),
			udpLimited, udpDeliveredBps, tcpLimited, tcpDeliveredBps, i.lane.currentFECRepairCount())
	}
}
```

Update existing send tests that call `OnQoSStatus` to pass repair count `1`.

- [ ] **Step 4: Decode repair count in RecvHandler**

In `internal/tunnel/v2/runtime/recvhandler.go`, add:

```go
func decodeLinkStatusState(state uint8) (bool, uint8, bool) {
	if state > 7 {
		return false, 0, false
	}
	return state&protocol.LinkStatusStateLimited != 0, ((state>>1)&0x07)+1, true
}

func decodeLinkStatusStatus(status uint8) (bool, bool, uint8, bool) {
	udpLimited, udpRepair, ok := decodeLinkStatusState(status >> 4)
	if !ok {
		return false, false, 0, false
	}
	tcpLimited, tcpRepair, ok := decodeLinkStatusState(status & 0x0f)
	if !ok || udpRepair != tcpRepair {
		return false, false, 0, false
	}
	return udpLimited, tcpLimited, udpRepair, true
}
```

Change `OnQoS`:

```go
udpLimited, tcpLimited, repairCount, ok := decodeLinkStatusStatus(body.Status)
if !ok {
	return protocol.ErrInvalidFrame
}
```

Change the QoS call:

```go
qos.OnQoSStatus(udpLimited, body.UDPDeliveredBps, tcpLimited, body.TCPDeliveredBps, repairCount)
```

- [ ] **Step 5: Run focused tests**

Run:

```bash
go test ./internal/tunnel/v2/send ./internal/tunnel/v2/runtime -run 'TestLaneQoS|TestRecvHandlerOnQoSFeedsQoSInput' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/v2/send/lanemanager.go internal/tunnel/v2/send/lane.go internal/tunnel/v2/send/lane_role_test.go internal/tunnel/v2/runtime/recvhandler.go internal/tunnel/v2/runtime/recvhandler_test.go
git commit -m "Route LINK_STATUS repair count into lane FEC"
```

---

### Task 4: Send Emits Multiple REPAIR Frames Per Group

**Files:**
- Modify: `internal/tunnel/v2/send/send.go`
- Modify: `internal/tunnel/v2/send/lane_role_test.go`
- Modify: `internal/tunnel/v2/send/e2e_test.go`

- [ ] **Step 1: Write failing send tests**

Add to `internal/tunnel/v2/send/lane_role_test.go`:

```go
func TestRepairCountEmitsMultipleRepairFrames(t *testing.T) {
	s := New()
	s.EnableFEC()
	sessionID := uint64(1)
	s.getSendState(sessionID).fecEnabled.Store(true)
	lane := newLaneRuntime(1, 100)
	lane.bindUDP(transport.LegRef{Kind: transport.KindUDP, EndpointID: "udp", RemoteAddr: &testAddr{addr: "127.0.0.1:9000"}})
	lane.bindTCP(transport.LegRef{Kind: transport.KindTCP, ConnID: "tcp"})
	lane.markActive(transport.KindUDP)
	lane.markActive(transport.KindTCP)
	lane.setFEC(3)
	s.lanes[laneKey{sessionID: sessionID, laneID: 1}] = lane

	group := txRepairGroup{
		basePacketID: 10,
		sourceSpan:   2,
		packets: []*packetbuf.Packet{
			packetbuf.Acquire(4),
			packetbuf.Acquire(4),
		},
	}
	copy(group.packets[0].Payload, []byte("aaaa"))
	copy(group.packets[1].Payload, []byte("bbbb"))
	s.sendRepair(context.Background(), sessionID, lane, group)

	var repairs []protocol.RepairBody
	for {
		select {
		case payload := <-s.Packets():
			frame, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && frame.Type == protocol.TypeREPAIR {
				if payload.Leg.Kind != transport.KindTCP {
					t.Fatalf("repair leg = %v, want TCP shadow", payload.Leg.Kind)
				}
				repairs = append(repairs, frame.Body.(protocol.RepairBody))
			}
		default:
			if len(repairs) != 3 {
				t.Fatalf("repairs = %d, want 3", len(repairs))
			}
			keys := map[uint16]struct{}{}
			for _, repair := range repairs {
				if repair.BasePacketID != 10 || repair.SourceSpan != 2 {
					t.Fatalf("repair = %+v, want group base 10 span 2", repair)
				}
				keys[repair.Key] = struct{}{}
			}
			if len(keys) != 3 {
				t.Fatalf("repair keys = %v, want 3 distinct keys", keys)
			}
			return
		}
	}
}
```

- [ ] **Step 2: Run focused test and confirm failure**

Run:

```bash
go test ./internal/tunnel/v2/send -run 'TestRepairCountEmitsMultipleRepairFrames|TestRepairUsesShadowLeg' -count=1
```

Expected: `TestRepairCountEmitsMultipleRepairFrames` gets one REPAIR frame.

- [ ] **Step 3: Update send codec cache**

In `internal/tunnel/v2/send/send.go`, replace codec fields:

```go
fecCodecs [maxFECSourceSpan + 1][5]*fec.Codec
```

Remove the standalone `fecCodec *fec.Codec` field.

In `EnableFEC`, initialize codecs:

```go
for span := 1; span <= maxFECSourceSpan; span++ {
	for repairs := 1; repairs <= 4; repairs++ {
		s.fecCodecs[span][repairs], _ = fec.NewCodec(span, repairs)
	}
}
```

Replace `fecCodecForSourceSpan`:

```go
func (s *Send) fecCodecFor(sourceSpan int, repairCount uint8) *fec.Codec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan || repairCount < 1 || repairCount > 4 {
		return nil
	}
	return s.fecCodecs[sourceSpan][repairCount]
}
```

- [ ] **Step 4: Emit one frame per repair shard**

In `sendRepair`, get count and keys:

```go
repairCount := lane.currentFECRepairCount()
codec := s.fecCodecFor(int(group.sourceSpan), repairCount)
if codec == nil {
	for _, pkt := range group.packets {
		pkt.Release()
	}
	return
}
keys := make([]uint16, repairCount)
for i := range keys {
	keys[i] = uint16(state.nextRepairKey.Add(1) - 1)
}
shards := make([][]byte, int(group.sourceSpan)+int(repairCount))
for i, pkt := range group.packets {
	shards[i] = pkt.Payload
}
if err := codec.Encode(shards, keys); err != nil {
	debuglog.Printf("send/fec", "encode_err session=%d lane=%d err=%v", sessionID, lane.id, err)
	for _, pkt := range group.packets {
		pkt.Release()
	}
	return
}
for _, pkt := range group.packets {
	pkt.Release()
}
```

Then send each repair:

```go
qosEnabled := s.qosSelectionEnabled()
leg := lane.shadowTransportWithQoS(qosEnabled)
if leg.Kind == 0 {
	return
}
for i, key := range keys {
	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeREPAIR,
		SessionID: sessionID,
		LaneID:    lane.id,
		Body: protocol.RepairBody{
			BasePacketID: group.basePacketID,
			Key:          key,
			SourceSpan:   group.sourceSpan,
			Symbol:       shards[int(group.sourceSpan)+i],
		},
	}
	packet, err := s.encodeFrame(frame)
	if err != nil {
		continue
	}
	if debuglog.Enabled() {
		primary := lane.primaryTransportWithQoS(qosEnabled)
		debuglog.Printf("send", "schedule_select session=%d lane=%d primary={%s} shadow={%s} leg={%s} frame=type=REPAIR base_packet_id=%d key=%d source_span=%d symbol_len=%d repair_index=%d repair_count=%d",
			sessionID, lane.id, debugLeg(primary), debugLeg(leg), debugLeg(leg),
			group.basePacketID, key, group.sourceSpan, len(shards[int(group.sourceSpan)+i]), i, repairCount)
	}
	_ = s.WriteTo(ctx, leg, packet)
}
```

- [ ] **Step 5: Run send tests**

Run:

```bash
go test ./internal/tunnel/v2/send -run 'TestRepairCountEmitsMultipleRepairFrames|TestRepairUsesShadowLeg|TestSendToRecvFECRecovery' -count=1
```

Expected: pass. `TestRepairUsesShadowLeg` still expects one REPAIR because default count is `1`.

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/v2/send/send.go internal/tunnel/v2/send/lane_role_test.go internal/tunnel/v2/send/e2e_test.go
git commit -m "Emit lane-local multi-repair frames"
```

---

### Task 5: Recv Reconstructs Multiple Missing DATA Packets

**Files:**
- Modify: `internal/tunnel/v2/recv/recv.go`
- Modify: `internal/tunnel/v2/recv/rx_group_window.go`
- Modify: `internal/tunnel/v2/recv/recv_test.go`

- [ ] **Step 1: Write failing recv recovery test**

Add a helper to `internal/tunnel/v2/recv/recv_test.go`:

```go
func ipv4Packet(payloadLen int, fill byte) []byte {
	if payloadLen < 20 {
		payloadLen = 20
	}
	packet := make([]byte, payloadLen)
	packet[0] = 0x45
	packet[2] = byte(payloadLen >> 8)
	packet[3] = byte(payloadLen)
	for i := 20; i < payloadLen; i++ {
		packet[i] = fill
	}
	return packet
}
```

Add the test:

```go
func TestRecvRecoversTwoMissingPacketsWithTwoRepairs(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(20); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	codec, err := fecpkg.NewCodec(4, 2)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	data := [][]byte{
		ipv4Packet(40, 'a'),
		ipv4Packet(44, 'b'),
		ipv4Packet(48, 'c'),
		ipv4Packet(52, 'd'),
		nil,
		nil,
	}
	keys := []uint16{30, 31}
	if err := codec.Encode(data, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, packetID := range []uint32{100, 102} {
		body := protocol.DataBody{PacketID: packetID, Packet: data[packetID-100]}
		frame := protocol.Frame{Type: protocol.TypeDATA, SessionID: 20, LaneID: 1, Body: body}
		if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA %d: %v", packetID, err)
		}
		readRecvPacket(t, out).Release()
	}
	for i, key := range keys {
		body := protocol.RepairBody{BasePacketID: 100, Key: key, SourceSpan: 4, Symbol: data[4+i]}
		frame := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 20, LaneID: 1, Body: body}
		if err := out.WriteTo(context.Background(), tcpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write REPAIR %d: %v", i, err)
		}
	}
	got1 := readRecvPacket(t, out)
	defer got1.Release()
	got2 := readRecvPacket(t, out)
	defer got2.Release()
	if string(got1.Payload[20:]) != string(data[1][20:]) && string(got2.Payload[20:]) != string(data[1][20:]) {
		t.Fatalf("missing packet 101 was not recovered")
	}
	if string(got1.Payload[20:]) != string(data[3][20:]) && string(got2.Payload[20:]) != string(data[3][20:]) {
		t.Fatalf("missing packet 103 was not recovered")
	}
	assertNoRecvPacket(t, out)
}
```

Add the `fecpkg` import:

```go
fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
```

- [ ] **Step 2: Run focused test and confirm failure**

Run:

```bash
go test ./internal/tunnel/v2/recv -run 'TestRecvRecoversTwoMissingPacketsWithTwoRepairs' -count=1
```

Expected: timeout or missing packet because `maybeRecover` still ignores groups with more than one missing DATA.

- [ ] **Step 3: Update recv codec interface and cache**

In `internal/tunnel/v2/recv/recv.go`, change the interface:

```go
type fecCodec interface {
	Reconstruct(shards [][]byte, keys []uint16) error
}
```

Change `Recv` codec cache:

```go
fecCodecs [maxFECSourceSpan + 1][5]fecCodec
```

Initialize in `New`:

```go
for sourceSpan := 1; sourceSpan <= maxFECSourceSpan; sourceSpan++ {
	for repairs := 1; repairs <= 4; repairs++ {
		out.fecCodecs[sourceSpan][repairs], _ = fecpkg.NewCodec(sourceSpan, repairs)
	}
}
```

Replace codec lookup:

```go
func (o *Recv) fecCodecFor(sourceSpan int, repairCount int) fecCodec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan || repairCount <= 0 || repairCount > 4 {
		return nil
	}
	return o.fecCodecs[sourceSpan][repairCount]
}
```

- [ ] **Step 4: Recover multiple packets**

Replace `maybeRecover` with:

```go
func (o *Recv) maybeRecover(ctx context.Context, sessionID uint64, laneID uint8, state *recvState, recoverable rxGroupRecoverable) error {
	missing := countMissing(recoverable.missingMask, recoverable.group.sourceSpan)
	if missing == 0 {
		return nil
	}
	codec := o.fecCodecFor(recoverable.group.sourceSpan, missing)
	if state == nil || codec == nil {
		return nil
	}
	packets, statuses, ok := o.recoverPackets(sessionID, laneID, state, recoverable, codec)
	if err := o.reportQoS(ctx, sessionID, laneID, statuses); err != nil {
		for _, pkt := range packets {
			pkt.Release()
		}
		return err
	}
	if !ok {
		return nil
	}
	for _, pkt := range packets {
		select {
		case o.packets <- pkt:
		case <-ctx.Done():
			pkt.Release()
			for _, rest := range packets {
				if rest != pkt {
					rest.Release()
				}
			}
			return ctx.Err()
		}
	}
	return nil
}
```

Rename and replace `recoverPacket`:

```go
func (o *Recv) recoverPackets(sessionID uint64, laneID uint8, state *recvState, recoverable rxGroupRecoverable, codec fecCodec) ([]*packetbuf.Packet, []qosStatus, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, nil, false
	}
	window := state.rxWindows[laneID]
	if window == nil {
		return nil, nil, false
	}
	shards, repairKeys, ok := window.buildShardsLocked(recoverable, state.shardScratch[:0], state.repairKeyScratch[:0])
	if !ok {
		return nil, nil, false
	}
	if err := codec.Reconstruct(shards, repairKeys); err != nil {
		debuglog.Printf("recv", "recover_err session=%d lane=%d base_packet_id=%d keys=%v source_span=%d err=%v",
			sessionID, laneID, recoverable.group.basePacketID, repairKeys, recoverable.group.sourceSpan, err)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "recover_err"),
			metrics.L("session", sessionID),
			metrics.L("source_span", recoverable.group.sourceSpan),
		)
		return nil, nil, false
	}
	var packets []*packetbuf.Packet
	for i := 0; i < recoverable.group.sourceSpan; i++ {
		if recoverable.missingMask&(1<<uint(i)) == 0 {
			continue
		}
		packetID := recoverable.group.basePacketID + uint32(i)
		payload, ipOK := recoveredIPv4Packet(shards[i])
		if !ipOK {
			for _, pkt := range packets {
				pkt.Release()
			}
			return nil, nil, false
		}
		if !state.dedupe.mark(packetID) {
			continue
		}
		pkt := packetbuf.Acquire(len(payload))
		copy(pkt.Payload, payload)
		packets = append(packets, pkt)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "recover_emit"),
			metrics.L("session", sessionID),
			metrics.L("source_span", recoverable.group.sourceSpan),
		)
		debuglog.Printf("recv", "recover_emit session=%d lane=%d packet_id=%d base_packet_id=%d keys=%v source_span=%d bytes=%d",
			sessionID, laneID, packetID, recoverable.group.basePacketID, repairKeys, recoverable.group.sourceSpan, len(payload))
	}
	result := window.finishRecovery(recoverable)
	statuses := o.observeGroupResult(state, laneID, result, time.Now())
	return packets, statuses, len(packets) > 0
}
```

Keep `rx_group_window.go` unchanged unless a helper is needed for missing indexes.

- [ ] **Step 5: Run recv focused tests**

Run:

```bash
go test ./internal/tunnel/v2/recv -run 'TestRecvRecoversTwoMissingPacketsWithTwoRepairs|TestRecvDuplicateDATAIsNotEmittedOrInsertedIntoWindow|TestRecvRepairCreatesGroupWindowEntry' -count=1
```

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add internal/tunnel/v2/recv/recv.go internal/tunnel/v2/recv/rx_group_window.go internal/tunnel/v2/recv/recv_test.go
git commit -m "Recover multiple DATA packets from FEC"
```

---

### Task 6: Recv Byte Accounting and Adaptive Repair Feedback

**Files:**
- Modify: `internal/tunnel/v2/recv/recv.go`
- Modify: `internal/tunnel/v2/recv/qos_estimator.go`
- Modify: `internal/tunnel/v2/recv/qos_estimator_test.go`
- Modify: `internal/tunnel/v2/recv/recv_test.go`
- Modify: `internal/tunnel/v2/runtime/qos_test.go`

- [ ] **Step 1: Write failing tests for repair count and no synthetic source bytes**

Add to `internal/tunnel/v2/recv/recv_test.go`:

```go
func TestRecvAdaptiveRepairCountUsesGroupLoss(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(30); !ok {
		t.Fatal("Create session failed")
	}
	var statuses []QoSStatus
	out := New(Config{
		SessionManager: &manager,
		OnQoSStatus: func(ctx context.Context, status QoSStatus) error {
			statuses = append(statuses, status)
			return nil
		},
	})
	state := out.recvState(30)
	state.mu.Lock()
	now := time.Now()
	result := rxGroupWindowResult{done: []rxGroupDone{{
		group:        rxGroupKey{basePacketID: 100, sourceSpan: 4},
		dataArrived:  2,
		dataExpected: 4,
		expired:      true,
	}}}
	state.fecGroups[rxLaneGroupKey{laneID: 1, group: rxGroupKey{basePacketID: 100, sourceSpan: 4}}] = rxGroupObservation{
		dataKind:   transport.KindUDP,
		repairKind: transport.KindTCP,
	}
	qosStatuses := out.observeGroupResult(state, 1, result, now)
	state.mu.Unlock()
	if err := out.reportQoS(context.Background(), 30, 1, qosStatuses); err != nil {
		t.Fatalf("reportQoS: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one repair-count status", statuses)
	}
	if statuses[0].RepairCount != 2 {
		t.Fatalf("repair count = %d, want 2", statuses[0].RepairCount)
	}
}
```

Add to `internal/tunnel/v2/recv/qos_estimator_test.go`:

```go
func TestQoSEstimatorHealthDoesNotCommitLimitedState(t *testing.T) {
	var got []qosStatus
	e := newQoSEstimator(qosConfig{SampleFloor: 1, Sustain: time.Second, Tick: time.Second}, func(status qosStatus) {
		got = append(got, status)
	})
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		e.ObserveHealth(qosHealthSample{
			At:           now.Add(time.Duration(i) * time.Second),
			DataKind:     transport.KindUDP,
			RepairKind:   transport.KindTCP,
			DataArrived:  0,
			DataExpected: 4,
		})
		got = append(got, e.Tick(now.Add(time.Duration(i+1)*time.Second))...)
	}
	if len(got) != 0 {
		t.Fatalf("health emitted QoS statuses = %+v, want none", got)
	}
}
```

- [ ] **Step 2: Run focused tests and confirm failure**

Run:

```bash
go test ./internal/tunnel/v2/recv -run 'TestRecvAdaptiveRepairCountUsesGroupLoss|TestQoSEstimatorHealthDoesNotCommitLimitedState' -count=1
```

Expected: compile failure for missing adaptive policy fields or a failing health-limited assertion.

- [ ] **Step 3: Add minimal adaptive policy state**

In `internal/tunnel/v2/recv/recv.go`, add:

```go
type rxFECPolicy struct {
	dataArrived  uint64
	dataExpected uint64
	repairCount  uint8
}

func (p *rxFECPolicy) current() uint8 {
	if p == nil || p.repairCount == 0 {
		return 1
	}
	return p.repairCount
}

func (p *rxFECPolicy) observe(done rxGroupDone) (uint8, bool) {
	if p == nil || done.dataExpected == 0 {
		return 1, false
	}
	if p.repairCount == 0 {
		p.repairCount = 1
	}
	p.dataArrived += uint64(done.dataArrived)
	p.dataExpected += uint64(done.dataExpected)
	missing := p.dataExpected - p.dataArrived
	target := uint8((missing*4 + p.dataExpected - 1) / p.dataExpected)
	if target < 1 {
		target = 1
	}
	if target > 4 {
		target = 4
	}
	if target == p.repairCount {
		return target, false
	}
	p.repairCount = target
	return target, true
}
```

Add to `recvState`:

```go
fecPolicies map[uint8]*rxFECPolicy
```

Initialize in `recvState` creation:

```go
fecPolicies: make(map[uint8]*rxFECPolicy),
```

Add helpers:

```go
func (s *recvState) fecPolicyFor(laneID uint8) *rxFECPolicy {
	p := s.fecPolicies[laneID]
	if p == nil {
		p = &rxFECPolicy{repairCount: 1}
		s.fecPolicies[laneID] = p
	}
	return p
}

func (s *recvState) attachRepairCount(laneID uint8, statuses []qosStatus) []qosStatus {
	if len(statuses) == 0 {
		return statuses
	}
	repairCount := s.fecPolicyFor(laneID).current()
	for i := range statuses {
		if statuses[i].RepairCount == 0 {
			statuses[i].RepairCount = repairCount
		}
	}
	return statuses
}
```

Add to internal `qosStatus` in `qos_estimator.go`:

```go
RepairCount uint8
```

Update `reportQoS` to expose it:

```go
RepairCount: status.RepairCount,
```

- [ ] **Step 4: Emit repair-count status from group outcomes**

In `observeGroupResult`, after health observation for each done group, add:

```go
policy := state.fecPolicyFor(laneID)
if repairCount, changed := policy.observe(done); changed {
	status := o.qosFor(state, laneID).snapshotStatus()
	status.RepairCount = repairCount
	statuses = append(statuses, status)
}
```

Add this method to `qos_estimator.go`:

```go
func (e *qosEstimator) snapshotStatus() qosStatus {
	if e == nil {
		return qosStatus{RepairCount: 1}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	status := e.snapshot()
	if status.RepairCount == 0 {
		status.RepairCount = 1
	}
	return status
}
```

Before every `reportQoS` call from `handleDATA`, `handleREPAIR`, `expireFECGroup`, and `recoverPackets`, call `state.attachRepairCount(laneID, statuses)` while holding `state.mu`.

In the async `qosFor` callback, attach repair count under `state.mu`:

```go
}, func(status qosStatus) {
	state.mu.Lock()
	statuses := state.attachRepairCount(laneID, []qosStatus{status})
	state.mu.Unlock()
	if err := o.reportQoS(context.Background(), sessionID, laneID, statuses); err != nil {
		debuglog.Printf("recv", "qos_report_error session=%d lane=%d err=%v", sessionID, laneID, err)
	}
})
```

- [ ] **Step 5: Stop FEC health from directly committing QoS limited**

In `qos_estimator.go`, change `evaluateDirection` tail:

```go
return qosStatus{}, false
```

Remove the call to `evaluateHealthState` from `evaluateDirection`. Keep `ObserveHealth` and health state storage because Recv still uses group outcomes for adaptive FEC. Existing tests that expected health-limited QoS should be updated to expect no QoS status and to validate adaptive repair status through Recv instead.

- [ ] **Step 6: Remove synthetic profile bytes from repair arrival**

In `observeRepairRate`, remove profile source bytes:

```go
return o.qosFor(state, laneID).ObserveRate(qosRateSample{
	At:          at,
	DataKind:    dataKind,
	RepairKind:  repairKind,
	RepairBytes: uint64(repairBytes),
})
```

Do not set `ProfileDataBytes` to `sourceSpan * repairBytes`.

Add source-byte profile accounting in `observeGroupResult` when recovered or complete group bytes are known. Use state-owned byte maps:

```go
type rxLanePacketKey struct {
	laneID   uint8
	packetID uint32
}
```

Add to `recvState`:

```go
fecDataBytes      map[rxLanePacketKey]uint64
fecRecoveredBytes map[rxLanePacketKey]uint64
```

On accepted DATA in `handleDATA`, store:

```go
state.fecDataBytes[rxLanePacketKey{laneID: frame.LaneID, packetID: body.PacketID}] = uint64(len(body.Packet))
```

In `recoverPackets`, after `recoveredIPv4Packet`, store:

```go
state.fecRecoveredBytes[rxLanePacketKey{laneID: laneID, packetID: packetID}] = uint64(len(payload))
```

Extend `rxGroupObservation`:

```go
repairBytes uint64
```

In `trackRepairGroup`, keep existing `dataKind` and `repairKind`; in `handleREPAIR`, add:

```go
obs := state.fecGroups[rxLaneGroupKey{laneID: frame.LaneID, group: group}]
obs.repairBytes += uint64(len(body.Symbol))
state.fecGroups[rxLaneGroupKey{laneID: frame.LaneID, group: group}] = obs
```

In `observeGroupResult`, compute profile bytes:

```go
actualDataBytes, recoveredBytes := state.groupSourceBytes(laneID, done.group)
if obs.repairBytes > 0 && actualDataBytes+recoveredBytes > 0 && !done.expired {
	statuses = append(statuses, o.qosFor(state, laneID).ObserveRate(qosRateSample{
		At:                 at,
		DataKind:           obs.dataKind,
		RepairKind:         obs.repairKind,
		ProfileDataBytes:   actualDataBytes + recoveredBytes,
		ProfileRepairBytes: obs.repairBytes,
	})...)
}
state.dropGroupSourceBytes(laneID, done.group)
```

Add helpers:

```go
func (s *recvState) groupSourceBytes(laneID uint8, group rxGroupKey) (uint64, uint64) {
	var actual uint64
	var recovered uint64
	for i := 0; i < group.sourceSpan; i++ {
		key := rxLanePacketKey{laneID: laneID, packetID: group.basePacketID + uint32(i)}
		actual += s.fecDataBytes[key]
		recovered += s.fecRecoveredBytes[key]
	}
	return actual, recovered
}

func (s *recvState) dropGroupSourceBytes(laneID uint8, group rxGroupKey) {
	for i := 0; i < group.sourceSpan; i++ {
		key := rxLanePacketKey{laneID: laneID, packetID: group.basePacketID + uint32(i)}
		delete(s.fecDataBytes, key)
		delete(s.fecRecoveredBytes, key)
	}
}
```

Initialize the maps in `recvState` creation.

- [ ] **Step 7: Run focused tests**

Run:

```bash
go test ./internal/tunnel/v2/recv -run 'TestRecvAdaptiveRepairCountUsesGroupLoss|TestQoSEstimatorHealthDoesNotCommitLimitedState|TestRecvReportsQoSStatusThroughCallback' -count=1
```

Expected: pass.

- [ ] **Step 8: Commit**

```bash
git add internal/tunnel/v2/recv/recv.go internal/tunnel/v2/recv/qos_estimator.go internal/tunnel/v2/recv/qos_estimator_test.go internal/tunnel/v2/recv/recv_test.go internal/tunnel/v2/runtime/qos_test.go
git commit -m "Drive repair count from FEC health"
```

---

### Task 7: End-to-End Validation and Protocol Docs

**Files:**
- Modify: `docs/protocol.md`
- Test: `internal/tunnel/v2/send/e2e_test.go`
- Test: all touched Go packages

- [ ] **Step 1: Update protocol documentation**

In `docs/protocol.md`, update the LINK_STATUS section so it states:

```text
LINK_STATUS.status = UUUU TTTT

Each nibble:
  bit0    = QoS limited state
  bits1-3 = repairCount - 1

repairCount is lane-local. The sender writes the same repair-count bits into
the UDP and TCP nibbles. The QoS bit remains transport-kind specific.
```

Replace any text that says FEC scope is not lane-local with:

```text
FEC groups are lane-local. A REPAIR frame can only repair DATA packets from the
same session and lane as the REPAIR frame.
```

Keep the existing statement that Runtime QoSWriter sends LINK_STATUS through `Send.WriteFrame` with a TCP ref.

- [ ] **Step 2: Run repository searches for old semantics**

Run:

```bash
rg -n "source_span \\* repair|SetFECRepairCount|repair count bits update lane FEC|FEC scope is not|LinkStatusStateLimited << 4|OnQoSStatus\\(" docs internal
```

Expected:
- No `SetFECRepairCount`.
- No `source_span * repair` source-byte accounting.
- `OnQoSStatus(` call sites all pass `repairCount`.
- Historical spec files may mention old text; `docs/protocol.md` and source files must match the new spec.

- [ ] **Step 3: Run focused package tests**

Run:

```bash
go test ./internal/fec ./internal/protocol ./internal/tunnel/v2/recv ./internal/tunnel/v2/send ./internal/tunnel/v2/runtime -count=1
```

Expected: pass.

- [ ] **Step 4: Run build**

Run:

```bash
go build ./...
```

Expected: pass.

- [ ] **Step 5: Run full tests**

Run:

```bash
go test ./...
```

Expected: pass.

- [ ] **Step 6: Inspect diff for boundary drift**

Run:

```bash
git diff -- internal/fec internal/protocol internal/tunnel/v2/recv internal/tunnel/v2/send internal/tunnel/v2/runtime docs/protocol.md
```

Verify:
- No public Send or LaneManager FEC setter was added.
- `lane.setFEC` is unexported.
- `rxGroupWindow` still has no QoS, policy, LINK_STATUS, transport kind, or time fields.
- Session and Transport APIs were not changed.
- LINK_STATUS still uses TCP ref in `QoSWriter`.

- [ ] **Step 7: Commit final docs and validation fixes**

```bash
git add docs/protocol.md internal/fec internal/protocol internal/tunnel/v2/recv internal/tunnel/v2/send internal/tunnel/v2/runtime
git commit -m "Validate multi-repair FEC data flow"
```

---

## Final Verification

Run:

```bash
git status --short
```

Expected: no tracked-file changes. The existing untracked `multipath` build artifact may remain untracked unless the user asks to remove it.

Run:

```bash
git log --oneline -7
```

Expected: commits for codec, protocol/runtime encoding, lane feedback, send multi-repair, recv multi-recovery, adaptive repair count, and final docs/validation.
