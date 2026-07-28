package recv

import (
	"bytes"
	"context"
	"testing"
	"time"

	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

type recordingHandler struct {
	calls                     []string
	bandwidthProbeObservation BandwidthProbeObservation
}

func (h *recordingHandler) OnHello(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnHello")
	return nil
}
func (h *recordingHandler) OnHelloAck(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnHelloAck")
	return nil
}
func (h *recordingHandler) OnPing(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnPing")
	return nil
}
func (h *recordingHandler) OnPong(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnPong")
	return nil
}
func (h *recordingHandler) OnClose(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnClose")
	return nil
}
func (h *recordingHandler) OnBandwidthProbe(ctx context.Context, leg Ref, frame protocol.Frame) (BandwidthProbeObservation, error) {
	h.calls = append(h.calls, "OnBandwidthProbe")
	return h.bandwidthProbeObservation, nil
}
func (h *recordingHandler) OnBandwidthProbeAck(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnBandwidthProbeAck")
	return nil
}
func (h *recordingHandler) OnQoS(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnQoS")
	return nil
}

func encodedTestFrame(t *testing.T, frame protocol.Frame) *packetbuf.Packet {
	t.Helper()
	payload, err := protocol.Encode(frame, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	return packet
}

func ipv4Packet(payloadLen int, fill byte) []byte {
	packet := make([]byte, 20+payloadLen)
	packet[0] = 0x45
	totalLen := len(packet)
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	for i := 20; i < len(packet); i++ {
		packet[i] = fill
	}
	return packet
}

func readRecvPacket(t *testing.T, out *Recv) *packetbuf.Packet {
	t.Helper()
	select {
	case packet := <-out.Packets():
		return packet
	default:
		t.Fatal("missing recv packet")
		return nil
	}
}

func assertNoRecvPacket(t *testing.T, out *Recv) {
	t.Helper()
	select {
	case packet := <-out.Packets():
		packet.Release()
		t.Fatal("unexpected recv packet")
	default:
	}
}

func qosPendingBytesFor(t *testing.T, q *qosEstimator, dataKind, repairKind transport.Kind) qosPendingBytes {
	t.Helper()
	if q == nil {
		t.Fatal("missing QoS estimator")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.directionLocked(dataKind, repairKind, q.currentRole)
	if state == nil {
		t.Fatalf("missing QoS direction for %v/%v", dataKind, repairKind)
	}
	return state.pending
}

func udpLeg() Ref {
	return Ref{Kind: transport.KindUDP, EndpointID: "ep"}
}

func tcpLeg() Ref {
	return Ref{Kind: transport.KindTCP, ConnID: "conn"}
}

func TestRecvDispatchesControlFramesToHandler(t *testing.T) {
	handler := &recordingHandler{}
	out := New(Config{Handler: handler})

	frames := []protocol.Frame{
		{Type: protocol.TypeHELLO, SessionID: 1, LaneID: 1, Body: protocol.HelloBody{Nonce: 1}},
		{Type: protocol.TypeHELLOACK, SessionID: 1, LaneID: 1, Body: protocol.HelloAckBody{Nonce: 1, Accepted: 1}},
		{Type: protocol.TypePING, SessionID: 1, LaneID: 1, Body: protocol.PingBody{PingID: 1}},
		{Type: protocol.TypePONG, SessionID: 1, LaneID: 1, Body: protocol.PingBody{PingID: 1}},
		{Type: protocol.TypeCLOSE, SessionID: 1, LaneID: 1, Body: protocol.CloseBody{Scope: protocol.CloseScopeLane}},
		{Type: protocol.TypeBandwidthProbe, SessionID: 1, LaneID: 1, Body: protocol.BandwidthProbeBody{Count: 1, TargetBps: 1}},
		{Type: protocol.TypeBandwidthProbeAck, SessionID: 1, LaneID: 1, Body: protocol.BandwidthProbeAckBody{Count: 1}},
		{Type: protocol.TypeLinkStatus, SessionID: 1, LaneID: 1, Body: protocol.LinkStatusBody{Status: 0x10, UDPDeliveredBps: 1}},
	}
	for _, frame := range frames {
		if err := out.Write(context.Background(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write %d: %v", frame.Type, err)
		}
	}

	want := []string{"OnHello", "OnHelloAck", "OnPing", "OnPong", "OnClose", "OnBandwidthProbe", "OnBandwidthProbeAck", "OnQoS"}
	if len(handler.calls) != len(want) {
		t.Fatalf("handler calls = %v, want %v", handler.calls, want)
	}
	for i := range want {
		if handler.calls[i] != want[i] {
			t.Fatalf("handler calls = %v, want %v", handler.calls, want)
		}
	}
}

func TestRecvKeepsDATAAndREPAIROutOfHandler(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		t.Fatal("Create session failed")
	}
	handler := &recordingHandler{}
	out := New(Config{Handler: handler, SessionManager: &manager})

	data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 99, LaneID: 1, Body: protocol.DataBody{GroupID: 1, Packet: []byte("p")}}
	if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, data)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 99, LaneID: 1, Body: protocol.RepairBody{GroupID: 1, Key: 1, SourceSpan: 4, Symbol: []byte("r")}}
	if err := out.WriteTo(context.Background(), tcpLeg(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	if len(handler.calls) != 0 {
		t.Fatalf("handler was called for DATA/REPAIR: %v", handler.calls)
	}
}

func TestRecvDuplicateDATAIsNotEmittedOrInsertedIntoWindow(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(7); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	mk := func() *packetbuf.Packet {
		return encodedTestFrame(t, protocol.Frame{
			Type: protocol.TypeDATA, SessionID: 7, LaneID: 1,
			Body: protocol.DataBody{GroupID: 42, SourceIndex: 1, Packet: []byte("packet")},
		})
	}

	// First arrival: emitted + accounted.
	if err := out.WriteTo(context.Background(), udpLeg(), mk()); err != nil {
		t.Fatalf("Write DATA 1: %v", err)
	}
	first := readRecvPacket(t, out)
	first.Release()

	// Duplicate: dropped before TUN.
	if err := out.WriteTo(context.Background(), udpLeg(), mk()); err != nil {
		t.Fatalf("Write DATA dup: %v", err)
	}
	assertNoRecvPacket(t, out)

	st := out.recvState(7)
	if st == nil {
		t.Fatal("missing recv state")
	}
	st.mu.Lock()
	window := st.rxWindows[1]
	var dataCount int
	if window != nil {
		if group, _ := window.groups.Get(42); group != nil {
			for _, packet := range group.data {
				if packet != nil {
					dataCount++
				}
			}
		}
	}
	st.mu.Unlock()
	if dataCount != 1 {
		t.Fatalf("window DATA entries = %d, want 1", dataCount)
	}
}

func TestRecvLateOriginalDATACountsQoSWithoutReemit(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(14); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	state := out.recvState(14)
	state.mu.Lock()
	state.qos[1] = newQoSEstimator(qosConfig{SessionID: 14, LaneID: 1, Tick: time.Hour}, nil)
	state.mu.Unlock()

	codec, err := fecpkg.NewCodec(2, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{
		ipv4Packet(12, 'a'),
		ipv4Packet(16, 'b'),
		nil,
	}
	keys := []uint16{7}
	if err := codec.Encode(shards, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	ctx := context.Background()
	sendDATA := func(sourceIndex uint8, packet []byte) {
		t.Helper()
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 14,
			LaneID:    1,
			Body: protocol.DataBody{
				GroupID:     100,
				SourceIndex: sourceIndex,
				Packet:      packet,
			},
		}
		if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA index %d: %v", sourceIndex, err)
		}
	}

	sendDATA(0, shards[0])
	first := readRecvPacket(t, out)
	if !bytes.Equal(first.Payload, shards[0]) {
		t.Fatalf("first packet = %v, want %v", first.Payload, shards[0])
	}
	first.Release()

	q := state.qos[1]
	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.originalDataBytes != uint64(len(shards[0])) ||
		got.expectedBytes != 0 ||
		got.repairBytes != 0 ||
		got.lateDataBytes != 0 {
		t.Fatalf("pending after first DATA = %+v, want original=%d only", got, len(shards[0]))
	}

	repair := protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 14,
		LaneID:    1,
		Body: protocol.RepairBody{
			GroupID:    100,
			Key:        keys[0],
			SourceSpan: 2,
			Symbol:     shards[2],
		},
	}
	if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}
	recovered := readRecvPacket(t, out)
	if !bytes.Equal(recovered.Payload, shards[1]) {
		t.Fatalf("recovered packet = %v, want %v", recovered.Payload, shards[1])
	}
	recovered.Release()

	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.originalDataBytes != uint64(len(shards[0])) ||
		got.expectedBytes != uint64(len(shards[0])+len(shards[1])) ||
		got.repairBytes != uint64(len(shards[2])) ||
		got.lateDataBytes != 0 {
		t.Fatalf("pending after recovery = %+v, want original=%d expected=%d repair=%d late=0",
			got, len(shards[0]), len(shards[0])+len(shards[1]), len(shards[2]))
	}

	sendDATA(1, shards[1])
	assertNoRecvPacket(t, out)
	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.originalDataBytes != uint64(len(shards[0])) ||
		got.expectedBytes != uint64(len(shards[0])+len(shards[1])) ||
		got.lateDataBytes != uint64(len(shards[1])) {
		t.Fatalf("pending after late original = %+v, want original=%d expected=%d late=%d",
			got, len(shards[0]), len(shards[0])+len(shards[1]), len(shards[1]))
	}

	sendDATA(1, shards[1])
	assertNoRecvPacket(t, out)
	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.originalDataBytes != uint64(len(shards[0])) ||
		got.expectedBytes != uint64(len(shards[0])+len(shards[1])) ||
		got.lateDataBytes != uint64(len(shards[1])) {
		t.Fatalf("pending after duplicate late original = %+v, want original=%d expected=%d late=%d",
			got, len(shards[0]), len(shards[0])+len(shards[1]), len(shards[1]))
	}
}

func TestRecvLateRepairAfterClosedGroupStillCountsQoSRepairBytes(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(15); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	state := out.recvState(15)
	state.mu.Lock()
	state.qos[1] = newQoSEstimator(qosConfig{SessionID: 15, LaneID: 1, Tick: time.Hour}, nil)
	state.mu.Unlock()

	codec, err := fecpkg.NewCodec(2, 2)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{
		ipv4Packet(12, 'a'),
		ipv4Packet(16, 'b'),
		nil,
		nil,
	}
	keys := []uint16{7, 8}
	if err := codec.Encode(shards, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 15,
			LaneID:    1,
			Body: protocol.DataBody{
				GroupID:     100,
				SourceIndex: uint8(i),
				Packet:      shards[i],
			},
		}
		if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA %d: %v", i, err)
		}
		readRecvPacket(t, out).Release()
	}

	writeRepair := func(key uint16, symbol []byte) {
		t.Helper()
		frame := protocol.Frame{
			Type:      protocol.TypeREPAIR,
			SessionID: 15,
			LaneID:    1,
			Body: protocol.RepairBody{
				GroupID:     100,
				Key:         key,
				SourceSpan:  2,
				RepairCount: 2,
				Symbol:      symbol,
			},
		}
		if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write REPAIR %d: %v", key, err)
		}
	}

	q := state.qos[1]
	writeRepair(keys[0], shards[2])
	firstRepairBytes := uint64(len(shards[2]))
	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.repairBytes != firstRepairBytes {
		t.Fatalf("pending after first repair = %+v, want repair=%d", got, firstRepairBytes)
	}

	writeRepair(keys[1], shards[3])
	wantRepairBytes := firstRepairBytes + uint64(len(shards[3]))
	if got := qosPendingBytesFor(t, q, transport.KindUDP, transport.KindTCP); got.repairBytes != wantRepairBytes {
		t.Fatalf("pending after late repair = %+v, want repair=%d", got, wantRepairBytes)
	}
}

func TestRecvPerLaneWindowIsolation(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(5); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	// DATA on lane 1, REPAIR on lane 2: different windows, no cross recovery.
	data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 5, LaneID: 1, Body: protocol.DataBody{GroupID: 10, Packet: []byte("packetdata")}}
	if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, data)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	readRecvPacket(t, out).Release()

	st := out.recvState(5)
	st.mu.Lock()
	_, lane1HasWindow := st.rxWindows[1]
	_, lane2HasWindow := st.rxWindows[2]
	st.mu.Unlock()

	if !lane1HasWindow {
		t.Error("lane 1 should have a window")
	}
	if lane2HasWindow {
		t.Error("lane 2 should not have a window (no frame on it)")
	}
}

func TestRecvDedupeIsLaneLocal(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(19)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	defer out.closeRecvState(19, sess)

	for laneID := uint8(1); laneID <= 2; laneID++ {
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 19,
			LaneID:    laneID,
			Body: protocol.DataBody{
				GroupID:     7,
				SourceIndex: 0,
				Packet:      []byte{laneID},
			},
		}
		if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA lane %d: %v", laneID, err)
		}
		packet := readRecvPacket(t, out)
		if len(packet.Payload) != 1 || packet.Payload[0] != laneID {
			t.Fatalf("lane %d packet = %v", laneID, packet.Payload)
		}
		packet.Release()
	}
}

func TestRecvRepairCreatesGroupWindowEntry(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(6); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	repair := protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 6,
		LaneID:    1,
		Body: protocol.RepairBody{
			GroupID:    10,
			Key:        3,
			SourceSpan: 2,
			Symbol:     []byte("repair"),
		},
	}
	if err := out.WriteTo(context.Background(), tcpLeg(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	st := out.recvState(6)
	st.mu.Lock()
	window := st.rxWindows[1]
	group, _ := window.groups.Get(10)
	st.mu.Unlock()
	if group == nil {
		t.Fatal("missing repair group")
	}
	if len(group.repairs) != 1 || group.repairs[0].key != 3 {
		t.Fatalf("repairs = %+v, want one key=3", group.repairs)
	}
}

func TestRecvRecoversTwoMissingPacketsWithTwoRepairs(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(8); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	codec, err := fecpkg.NewCodec(4, 2)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{
		ipv4Packet(14, 'a'),
		ipv4Packet(5, 'b'),
		ipv4Packet(13, 'c'),
		ipv4Packet(7, 'd'),
		nil,
		nil,
	}
	keys := []uint16{7, 8}
	if err := codec.Encode(shards, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	ctx := context.Background()
	sendData := func(sourceIndex uint8, packet []byte) {
		t.Helper()
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 8,
			LaneID:    1,
			Body: protocol.DataBody{
				GroupID:     100,
				SourceIndex: sourceIndex,
				Packet:      packet,
			},
		}
		if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA index %d: %v", sourceIndex, err)
		}
		got := readRecvPacket(t, out)
		defer got.Release()
		if !bytes.Equal(got.Payload, packet) {
			t.Fatalf("DATA index %d payload len/content mismatch", sourceIndex)
		}
	}
	sendData(0, shards[0])
	sendData(2, shards[2])

	for i, key := range keys {
		repair := protocol.Frame{
			Type:      protocol.TypeREPAIR,
			SessionID: 8,
			LaneID:    1,
			Body: protocol.RepairBody{
				GroupID:    100,
				Key:        key,
				SourceSpan: 4,
				Symbol:     shards[4+i],
			},
		}
		if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, repair)); err != nil {
			t.Fatalf("Write REPAIR %d: %v", i, err)
		}
	}

	recovered101 := readRecvPacket(t, out)
	if !bytes.Equal(recovered101.Payload, shards[1]) {
		t.Fatalf("recovered packet 101 = len %d %v, want len %d %v",
			len(recovered101.Payload), recovered101.Payload, len(shards[1]), shards[1])
	}
	recovered101.Release()

	recovered103 := readRecvPacket(t, out)
	if !bytes.Equal(recovered103.Payload, shards[3]) {
		t.Fatalf("recovered packet 103 = len %d %v, want len %d %v",
			len(recovered103.Payload), recovered103.Payload, len(shards[3]), shards[3])
	}
	recovered103.Release()
	assertNoRecvPacket(t, out)
}

func TestRecvReportsQoSStatusThroughCallback(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(10); !ok {
		t.Fatal("Create session failed")
	}
	var statuses []QoSStatus
	ctx := context.Background()
	out := New(Config{
		SessionManager: &manager,
		OnQoSStatus: func(ctx context.Context, status QoSStatus) error {
			statuses = append(statuses, status)
			return nil
		},
	})
	qosStatuses := []qosStatus{{UDPLimited: true, RepairCount: 1}}
	if err := out.reportQoS(ctx, 10, 1, qosStatuses); err != nil {
		t.Fatalf("reportQoS: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one", statuses)
	}
	if statuses[0].SessionID != 10 || statuses[0].LaneID != 1 || !statuses[0].UDPLimited || statuses[0].TCPLimited || statuses[0].RepairCount != 1 {
		t.Fatalf("status = %+v, want UDP limited for session 10 lane 1 with repair count 1", statuses[0])
	}
}

func TestRecvAppliesBandwidthProbeObservationFromHandler(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(11); !ok {
		t.Fatal("Create session failed")
	}
	handler := &recordingHandler{
		bandwidthProbeObservation: BandwidthProbeObservation{
			Primary:         transport.KindTCP,
			UDPLimited:      true,
			UDPDeliveredBps: 20_000_000,
			TCPDeliveredBps: 80_000_000,
			CapBps:          200_000_000,
		},
	}
	var statuses []QoSStatus
	out := New(Config{
		Handler:        handler,
		SessionManager: &manager,
		OnQoSStatus: func(ctx context.Context, status QoSStatus) error {
			statuses = append(statuses, status)
			return nil
		},
	})

	frame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeBandwidthProbe,
		SessionID: 11,
		LaneID:    2,
		Body: protocol.BandwidthProbeBody{
			TrainID:             10,
			ProbeID:             10,
			Count:               1,
			TargetBps:           200_000_000,
			TrainBytesRemaining: 0,
		},
	}
	if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, frame)); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one", statuses)
	}
	status := statuses[0]
	if status.SessionID != 11 || status.LaneID != 2 || !status.UDPLimited || status.TCPLimited {
		t.Fatalf("status = %+v, want UDP limited for session 11 lane 2", status)
	}
	if status.UDPDeliveredBps != 20_000_000 || status.TCPDeliveredBps != 80_000_000 {
		t.Fatalf("status delivered bps udp=%d tcp=%d, want udp=20000000 tcp=80000000", status.UDPDeliveredBps, status.TCPDeliveredBps)
	}

	state := out.recvState(11)
	if state == nil {
		t.Fatal("missing recv state")
	}
	state.mu.Lock()
	q := state.qos[2]
	if q == nil {
		state.mu.Unlock()
		t.Fatal("missing QoS estimator")
	}
	q.mu.Lock()
	capBps := q.tcpUDPRepair.repairLoadCapBps
	q.mu.Unlock()
	state.mu.Unlock()
	if capBps != 200_000_000 {
		t.Fatalf("repair load cap bps = %d, want 200000000", capBps)
	}
}

func TestRxGroupWindowFinishesRecoveredGroup(t *testing.T) {
	w := newRxGroupWindow()
	result, _ := w.addRepair(100, 7, 1, ipv4Packet(20, 'r'))
	if len(result.recoverable) != 1 {
		t.Fatalf("recoverable = %+v, want one group", result.recoverable)
	}

	result = w.finishRecovery(result.recoverable[0], uint64(len(ipv4Packet(20, 'r'))), uint64(len(ipv4Packet(20, 'r'))))
	if len(result.done) != 1 {
		t.Fatalf("done after recovery = %+v, want one done group", result.done)
	}
	if !result.done[0].recovered || result.done[0].expired {
		t.Fatalf("done = %+v, want recovered result without expired", result.done[0])
	}
	if group, _ := w.groups.Get(100); group != nil {
		t.Fatalf("group after recovery = %+v, want dropped", group)
	}
}

func TestRxGroupWindowExpireCompletesGroupBeforeExpired(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	w.addData(100, 0, ipv4Packet(10, 'a'))
	w.addData(100, 1, ipv4Packet(12, 'b'))
	key := rxGroupKey{groupID: 100, sourceSpan: 2}
	group, _ := w.groups.Get(100)
	group.key = key

	result := w.expireGroup(100)
	if len(result.done) != 1 {
		t.Fatalf("done = %+v, want one completed group", result.done)
	}
	if result.done[0].expired || result.done[0].recovered {
		t.Fatalf("done = %+v, want completed group without expired/recovered", result.done[0])
	}
	if group, _ := w.groups.Get(100); group != nil {
		t.Fatalf("group after complete expire = %+v, want dropped", group)
	}
}

func TestRxGroupWindowExpireReturnsRecoverableGroup(t *testing.T) {
	w := newRxGroupWindow()
	defer w.releaseAll()

	result, _ := w.addRepair(100, 7, 1, ipv4Packet(20, 'r'))
	if len(result.recoverable) != 1 {
		t.Fatalf("recoverable after repair = %+v, want one group", result.recoverable)
	}

	result = w.expireGroup(100)
	if len(result.recoverable) != 1 || len(result.done) != 0 {
		t.Fatalf("expire result = %+v, want recoverable without done", result)
	}
	if group, _ := w.groups.Get(100); group == nil {
		t.Fatal("recoverable group was dropped before recovery")
	}
}

func TestRecvExpireFECGroupRecoversRecoverableGroup(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(16)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	defer out.closeRecvState(16, sess)

	codec, err := fecpkg.NewCodec(2, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{
		ipv4Packet(10, 'a'),
		ipv4Packet(12, 'b'),
		nil,
	}
	keys := []uint16{7}
	if err := codec.Encode(shards, keys); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	state := out.recvState(16)
	group := rxGroupKey{groupID: 100, sourceSpan: 2}
	state.mu.Lock()
	state.qos[1] = newQoSEstimator(qosConfig{SessionID: 16, LaneID: 1, Tick: time.Hour}, nil)
	window := state.windowFor(1)
	window.addData(100, 0, shards[0])
	window.addRepair(100, keys[0], 2, shards[2])
	state.mu.Unlock()

	out.expireFECGroup(state, 1, group.groupID, nil)

	recovered := readRecvPacket(t, out)
	defer recovered.Release()
	if !bytes.Equal(recovered.Payload, shards[1]) {
		t.Fatalf("recovered packet = %v, want %v", recovered.Payload, shards[1])
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if got, _ := state.rxWindows[1].groups.Get(group.groupID); got != nil {
		t.Fatalf("group after expire recovery = %+v, want dropped", got)
	}
}

func TestRecvOldFECTimerCannotExpireReplacementGroupTimer(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(17)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	defer out.closeRecvState(17, sess)

	state := out.recvState(17)
	key := rxLaneGroupKey{laneID: 1, groupID: 100}
	oldTimer := time.NewTimer(time.Hour)
	newTimer := time.NewTimer(time.Hour)
	defer oldTimer.Stop()
	defer newTimer.Stop()

	state.mu.Lock()
	state.windowFor(1).addData(100, 0, []byte("data"))
	state.fecTimers[key] = newTimer
	state.mu.Unlock()

	out.expireFECGroup(state, 1, 100, oldTimer)

	state.mu.Lock()
	group, _ := state.rxWindows[1].groups.Get(100)
	tracked := state.fecTimers[key]
	state.mu.Unlock()
	if group == nil {
		t.Fatal("old timer expired the replacement group")
	}
	if tracked != newTimer {
		t.Fatal("old timer removed the replacement timer")
	}

	out.expireFECGroup(state, 1, 100, newTimer)
	state.mu.Lock()
	group, _ = state.rxWindows[1].groups.Get(100)
	_, trackedAfterExpire := state.fecTimers[key]
	state.mu.Unlock()
	if group != nil || trackedAfterExpire {
		t.Fatalf("replacement timer expiration left group=%+v tracked=%t", group, trackedAfterExpire)
	}
}

func TestRecvFECGroupTimerResetsInPlace(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(22)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	defer out.closeRecvState(22, sess)

	state := out.recvState(22)
	key := rxLaneGroupKey{laneID: 1, groupID: 100}
	state.mu.Lock()
	out.trackFECGroup(state, 1, 100)
	first := state.fecTimers[key]
	out.trackFECGroup(state, 1, 100)
	second := state.fecTimers[key]
	state.mu.Unlock()

	if first == nil || second != first {
		t.Fatalf("group timer first=%p second=%p, want one reset timer", first, second)
	}
}

func TestRecvConflictingRepairSpanDoesNotCreateQoSGroup(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(18)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	defer out.closeRecvState(18, sess)

	writeRepair := func(key uint16, sourceSpan uint8) {
		t.Helper()
		frame := protocol.Frame{
			Type:      protocol.TypeREPAIR,
			SessionID: 18,
			LaneID:    1,
			Body: protocol.RepairBody{
				GroupID:     100,
				Key:         key,
				SourceSpan:  sourceSpan,
				RepairCount: 1,
				Symbol:      []byte("repair"),
			},
		}
		if err := out.WriteTo(context.Background(), tcpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write REPAIR span=%d: %v", sourceSpan, err)
		}
	}

	writeRepair(7, 2)
	state := out.recvState(18)
	timerKey := rxLaneGroupKey{laneID: 1, groupID: 100}
	state.mu.Lock()
	firstTimer := state.fecTimers[timerKey]
	state.mu.Unlock()

	writeRepair(8, 4)

	state.mu.Lock()
	q := state.qos[1]
	trackedTimer := state.fecTimers[timerKey]
	state.mu.Unlock()
	q.mu.Lock()
	_, canonical := q.groups[rxGroupKey{groupID: 100, sourceSpan: 2}]
	_, conflicting := q.groups[rxGroupKey{groupID: 100, sourceSpan: 4}]
	groupCount := len(q.groups)
	q.mu.Unlock()

	if !canonical || conflicting || groupCount != 1 {
		t.Fatalf("QoS groups canonical=%t conflicting=%t count=%d, want true false 1", canonical, conflicting, groupCount)
	}
	if trackedTimer != firstTimer {
		t.Fatal("conflicting REPAIR reset the accepted group's timer")
	}
}

func TestRecvCloseStateConsumesSessionOnce(t *testing.T) {
	var manager session.Manager
	sess, ok := manager.Create(12)
	if !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	if st := out.recvState(12); st == nil {
		t.Fatal("missing recv state")
	}

	const workers = 16
	start := make(chan struct{})
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			out.closeRecvState(12, sess)
			done <- struct{}{}
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		<-done
	}

	if _, known := manager.Get(12); known {
		t.Fatal("session survived concurrent closeRecvState")
	}
	out.statesMu.RLock()
	_, exists := out.states[sess]
	out.statesMu.RUnlock()
	if exists {
		t.Fatal("recv state survived concurrent closeRecvState")
	}
}

func TestRecvDropsDataForUnknownSession(t *testing.T) {
	out := New()
	packet := encodedTestFrame(t, protocol.Frame{
		Type: protocol.TypeDATA, SessionID: 99, LaneID: 1,
		Body: protocol.DataBody{GroupID: 1, Packet: []byte("packet")},
	})
	if err := out.WriteTo(context.Background(), udpLeg(), packet); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	assertNoRecvPacket(t, out)
}
