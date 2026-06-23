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
	calls []string
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
func (h *recordingHandler) OnBandwidthProbe(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnBandwidthProbe")
	return nil
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
		{Type: protocol.TypeBandwidthProbe, SessionID: 1, LaneID: 1, Body: protocol.BandwidthProbeBody{Count: 1, TrainBytesTotal: 1}},
		{Type: protocol.TypeBandwidthProbeAck, SessionID: 1, LaneID: 1, Body: protocol.BandwidthProbeAckBody{Count: 1}},
		{Type: protocol.TypeLinkStatus, SessionID: 1, LaneID: 1, Body: protocol.LinkStatusBody{Status: protocol.LinkStatusStateLimited << 4, UDPDeliveredBps: 1}},
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

	data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 99, LaneID: 1, Body: protocol.DataBody{PacketID: 1, Packet: []byte("p")}}
	if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, data)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 99, LaneID: 1, Body: protocol.RepairBody{Key: 1, SourceSpan: 4, Symbol: []byte("r")}}
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
			Body: protocol.DataBody{PacketID: 42, Packet: []byte("packet")},
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
		dataCount = len(window.recentData)
	}
	st.mu.Unlock()
	if dataCount != 1 {
		t.Fatalf("window DATA entries = %d, want 1", dataCount)
	}
}

func TestRecvDoesNotStoreFECBytesForUngroupedDATA(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(17); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	frame := protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 17,
		LaneID:    1,
		Body: protocol.DataBody{
			PacketID: 1,
			Packet:   []byte("packet"),
		},
	}
	if err := out.WriteTo(context.Background(), udpLeg(), encodedTestFrame(t, frame)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	readRecvPacket(t, out).Release()

	state := out.recvState(17)
	state.mu.Lock()
	got := len(state.fecDataBytes)
	state.mu.Unlock()
	if got != 0 {
		t.Fatalf("FEC data byte entries = %d, want none without a repair group", got)
	}
}

func TestRecvPerLaneWindowIsolation(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(5); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	// DATA on lane 1, REPAIR on lane 2: different windows, no cross recovery.
	data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 5, LaneID: 1, Body: protocol.DataBody{PacketID: 10, Packet: []byte("packetdata")}}
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
			BasePacketID: 10,
			Key:          3,
			SourceSpan:   2,
			Symbol:       []byte("repair"),
		},
	}
	if err := out.WriteTo(context.Background(), tcpLeg(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	st := out.recvState(6)
	st.mu.Lock()
	window := st.rxWindows[1]
	group := window.groups[rxGroupKey{basePacketID: 10, sourceSpan: 2}]
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
	sendData := func(packetID uint32, packet []byte) {
		t.Helper()
		frame := protocol.Frame{
			Type:      protocol.TypeDATA,
			SessionID: 8,
			LaneID:    1,
			Body: protocol.DataBody{
				PacketID: packetID,
				Packet:   packet,
			},
		}
		if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write DATA %d: %v", packetID, err)
		}
		got := readRecvPacket(t, out)
		defer got.Release()
		if !bytes.Equal(got.Payload, packet) {
			t.Fatalf("DATA %d payload len/content mismatch", packetID)
		}
	}
	sendData(100, shards[0])
	sendData(102, shards[2])

	for i, key := range keys {
		repair := protocol.Frame{
			Type:      protocol.TypeREPAIR,
			SessionID: 8,
			LaneID:    1,
			Body: protocol.RepairBody{
				BasePacketID: 100,
				Key:          key,
				SourceSpan:   4,
				Symbol:       shards[4+i],
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
	state := out.recvState(10)
	state.mu.Lock()
	qosStatuses := []qosStatus{{UDPLimited: true}}
	state.attachRepairCount(1, qosStatuses)
	state.mu.Unlock()
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

func TestRecvAdaptiveRepairCountUsesGroupLoss(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(11); !ok {
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

	state := out.recvState(11)
	group := rxGroupKey{basePacketID: 100, sourceSpan: 4}
	state.mu.Lock()
	state.fecGroups[rxLaneGroupKey{laneID: 1, group: group}] = rxGroupObservation{
		dataKind:   transport.KindUDP,
		repairKind: transport.KindTCP,
	}
	qosStatuses := out.observeGroupResult(state, 1, rxGroupWindowResult{done: []rxGroupDone{{
		group:        group,
		dataArrived:  2,
		dataExpected: 4,
		expired:      true,
	}}}, time.Now())
	state.attachRepairCount(1, qosStatuses)
	state.mu.Unlock()
	if err := out.reportQoS(ctx, 11, 1, qosStatuses); err != nil {
		t.Fatalf("reportQoS: %v", err)
	}

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one", statuses)
	}
	if statuses[0].RepairCount != 2 {
		t.Fatalf("status = %+v, want repair count 2", statuses[0])
	}
	if statuses[0].UDPLimited || statuses[0].TCPLimited {
		t.Fatalf("status = %+v, want repair-only QoS snapshot", statuses[0])
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
		Body: protocol.DataBody{PacketID: 1, Packet: []byte("packet")},
	})
	if err := out.WriteTo(context.Background(), udpLeg(), packet); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	assertNoRecvPacket(t, out)
}
