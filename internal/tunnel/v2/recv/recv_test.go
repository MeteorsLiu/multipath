package recv

import (
	"context"
	"testing"
	"time"

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
		{Type: protocol.TypeLinkStatus, SessionID: 1, LaneID: 1, Body: protocol.LinkStatusBody{LegKind: protocol.LinkStatusLegUDP, Reason: protocol.LinkStatusReasonLimited, DeliveredBps: 1}},
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
		dataCount = len(window.data)
	}
	st.mu.Unlock()
	if dataCount != 1 {
		t.Fatalf("window DATA entries = %d, want 1", dataCount)
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
	state.qos[1] = newQoSEstimator(qosConfig{
		Sustain:     time.Second,
		SampleFloor: 4,
		Refresh:     time.Second,
	}, func(status qosStatus) {
		statuses = append(statuses, QoSStatus{
			SessionID:    10,
			LaneID:       1,
			Kind:         status.Kind,
			Reason:       status.Reason,
			DeliveredBps: status.DeliveredBps,
		})
	})
	state.mu.Unlock()

	for group := uint32(0); group < 4; group++ {
		repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 10, LaneID: 1, Body: protocol.RepairBody{BasePacketID: group * 4, Key: uint16(group), SourceSpan: 4, Symbol: []byte("rrrr")}}
		if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, repair)); err != nil {
			t.Fatalf("Write REPAIR %d: %v", group, err)
		}
	}
	state.mu.Lock()
	state.qos[1].Observe(qosSample{
		At:             time.Now(),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    0,
		DataExpected:   16,
		RecoveredBytes: 16 * 1200,
		RepairBytes:    4 * 1200,
	})
	state.qos[1].Observe(qosSample{
		At:             time.Now().Add(2 * time.Second),
		Duration:       time.Second,
		DataKind:       transport.KindUDP,
		RepairKind:     transport.KindTCP,
		DataArrived:    0,
		DataExpected:   16,
		RecoveredBytes: 16 * 1200,
		RepairBytes:    4 * 1200,
	})
	state.mu.Unlock()

	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one", statuses)
	}
	if statuses[0].SessionID != 10 || statuses[0].LaneID != 1 || statuses[0].Kind != transport.KindUDP || statuses[0].Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status = %+v, want UDP limited for session 10 lane 1", statuses[0])
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
