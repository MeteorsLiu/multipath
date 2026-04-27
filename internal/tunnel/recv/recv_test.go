package recv

import (
	"context"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
)

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
