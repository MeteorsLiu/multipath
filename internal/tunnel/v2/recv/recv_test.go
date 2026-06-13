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
func (h *recordingHandler) OnLinkStatus(ctx context.Context, leg Ref, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnLinkStatus")
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

	want := []string{"OnHello", "OnHelloAck", "OnPing", "OnPong", "OnClose", "OnBandwidthProbe", "OnBandwidthProbeAck", "OnLinkStatus"}
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

func TestRecvDuplicateDATADroppedAndNotAccounted(t *testing.T) {
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

	// Accounting: DATA on UDP counted exactly once (duplicate not counted).
	st := out.statsForTest(7, 1)
	if st == nil {
		t.Fatal("missing lane stats")
	}
	got := st.arrival(transport.KindUDP, catData)
	if got.count != 1 {
		t.Errorf("DATA count = %d, want 1 (duplicate must not be accounted)", got.count)
	}
	if got.bytes != uint64(len("packet")) {
		t.Errorf("DATA bytes = %d, want %d", got.bytes, len("packet"))
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

func TestRecvAccountingByKindAndCategory(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(8); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	ctx := context.Background()

	// 3 DATA on UDP (lane 1), distinct packet ids.
	for i := uint32(0); i < 3; i++ {
		f := protocol.Frame{Type: protocol.TypeDATA, SessionID: 8, LaneID: 1, Body: protocol.DataBody{PacketID: i, Packet: []byte("dddd")}}
		if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, f)); err != nil {
			t.Fatalf("Write DATA %d: %v", i, err)
		}
		readRecvPacket(t, out).Release()
	}
	// 2 REPAIR on TCP (lane 1).
	for i := uint32(0); i < 2; i++ {
		f := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 8, LaneID: 1, Body: protocol.RepairBody{BasePacketID: i * 4, Key: uint16(i), SourceSpan: 4, Symbol: []byte("rr")}}
		if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, f)); err != nil {
			t.Fatalf("Write REPAIR %d: %v", i, err)
		}
	}

	st := out.statsForTest(8, 1)
	if st == nil {
		t.Fatal("missing lane stats")
	}
	if d := st.arrival(transport.KindUDP, catData); d.count != 3 || d.bytes != 12 {
		t.Errorf("UDP DATA = %+v, want count=3 bytes=12", d)
	}
	if r := st.arrival(transport.KindTCP, catRepair); r.count != 2 || r.bytes != 4 {
		t.Errorf("TCP REPAIR = %+v, want count=2 bytes=4", r)
	}
	// Cross cells stay zero.
	if d := st.arrival(transport.KindTCP, catData); d.count != 0 {
		t.Errorf("TCP DATA count = %d, want 0", d.count)
	}
}

func TestRecvSingleTransportSameKind(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(9); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	ctx := context.Background()

	// Degraded lane: DATA and REPAIR both arrive on UDP.
	data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 9, LaneID: 1, Body: protocol.DataBody{PacketID: 0, Packet: []byte("dddd")}}
	if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, data)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	readRecvPacket(t, out).Release()
	repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 9, LaneID: 1, Body: protocol.RepairBody{BasePacketID: 0, Key: 0, SourceSpan: 4, Symbol: []byte("rr")}}
	if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	st := out.statsForTest(9, 1)
	// Both categories landed on UDP (kind index 0); TCP cells empty -> the
	// reserved single-transport precondition that QoS detection will check.
	if st.arrival(transport.KindUDP, catData).count != 1 {
		t.Error("UDP DATA count != 1")
	}
	if st.arrival(transport.KindUDP, catRepair).count != 1 {
		t.Error("UDP REPAIR count != 1")
	}
	if st.arrival(transport.KindTCP, catData).count != 0 || st.arrival(transport.KindTCP, catRepair).count != 0 {
		t.Error("TCP cells should be empty in single-UDP-transport case")
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

func TestRecvEmitsLinkStatusWhenQoSWindowDetectsLimit(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(77); !ok {
		t.Fatal("Create session failed")
	}
	var statuses []protocol.Frame
	now := time.Unix(0, 0)
	out := New(Config{
		SessionManager: &manager,
		LinkStatus: func(ctx context.Context, leg Ref, frame protocol.Frame) error {
			statuses = append(statuses, frame)
			return nil
		},
		QoSConfigForTest: qosConfig{
			Window:      time.Hour,
			Sustain:     time.Millisecond,
			SampleFloor: 4,
			Now:         func() time.Time { return now },
		},
	})
	ctx := context.Background()
	for group := uint32(0); group < 8; group++ {
		now = now.Add(time.Millisecond)
		base := group * 4
		repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 77, LaneID: 1, Body: protocol.RepairBody{BasePacketID: base, Key: uint16(group), SourceSpan: 4, Symbol: []byte("rrrr")}}
		if err := out.WriteTo(ctx, tcpLeg(), encodedTestFrame(t, repair)); err != nil {
			t.Fatalf("Write REPAIR: %v", err)
		}
		if group%4 == 0 {
			data := protocol.Frame{Type: protocol.TypeDATA, SessionID: 77, LaneID: 1, Body: protocol.DataBody{PacketID: base, Packet: []byte("data")}}
			if err := out.WriteTo(ctx, udpLeg(), encodedTestFrame(t, data)); err != nil {
				t.Fatalf("Write DATA: %v", err)
			}
			readRecvPacket(t, out).Release()
		}
	}
	if len(statuses) == 0 {
		t.Fatal("expected at least one LINK_STATUS")
	}
	body := statuses[len(statuses)-1].Body.(protocol.LinkStatusBody)
	if body.LegKind != protocol.LinkStatusLegUDP || body.Reason != protocol.LinkStatusReasonLimited {
		t.Fatalf("status body = %+v, want UDP limited", body)
	}
}

// statsForTest exposes a lane's arrival ledger for white-box assertions.
func (o *Recv) statsForTest(sessionID uint64, laneID uint8) *laneArrivalStats {
	st := o.recvState(sessionID)
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.accounting[laneID]
}
