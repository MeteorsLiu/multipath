package recv

import (
	"context"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

type recordingHandler struct {
	calls []string
}

func (h *recordingHandler) OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnHello")
	return nil
}

func (h *recordingHandler) OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnHelloAck")
	return nil
}

func (h *recordingHandler) OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnPing")
	return nil
}

func (h *recordingHandler) OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnPong")
	return nil
}

func (h *recordingHandler) OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnClose")
	return nil
}

func (h *recordingHandler) OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnBandwidthProbe")
	return nil
}

func (h *recordingHandler) OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	h.calls = append(h.calls, "OnBandwidthProbeAck")
	return nil
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
	}
	for _, frame := range frames {
		if err := out.Write(context.Background(), encodedTestFrame(t, frame)); err != nil {
			t.Fatalf("Write %d: %v", frame.Type, err)
		}
	}

	want := []string{"OnHello", "OnHelloAck", "OnPing", "OnPong", "OnClose", "OnBandwidthProbe", "OnBandwidthProbeAck"}
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
	if err := out.Write(context.Background(), encodedTestFrame(t, data)); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	repair := protocol.Frame{Type: protocol.TypeREPAIR, SessionID: 99, LaneID: 1, Body: protocol.RepairBody{Key: 1, SourceSpan: 4, Symbol: []byte("r")}}
	if err := out.Write(context.Background(), encodedTestFrame(t, repair)); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	if len(handler.calls) != 0 {
		t.Fatalf("handler was called for DATA/REPAIR: %v", handler.calls)
	}
}

func TestRecvDropsDataForUnknownSession(t *testing.T) {
	out := New()
	packet := encodedTestFrame(t, protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    1,
		Body:      protocol.DataBody{PacketID: 1, Packet: []byte("packet")},
	})

	if err := out.Write(context.Background(), packet); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	assertNoRecvPacket(t, out)
}

func TestRecvAcceptsDataForAdmittedSession(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})
	packet := encodedTestFrame(t, protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    1,
		Body:      protocol.DataBody{PacketID: 1, Packet: []byte("packet")},
	})

	if err := out.Write(context.Background(), packet); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}
	got := readRecvPacket(t, out)
	defer got.Release()
	if string(got.Payload) != "packet" {
		t.Fatalf("recv payload = %q, want packet", got.Payload)
	}
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
