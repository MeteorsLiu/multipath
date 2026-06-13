package runtime_test

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/runtime"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

type fecAddr struct{ addr string }

func (a *fecAddr) Network() string { return "udp" }
func (a *fecAddr) String() string  { return a.addr }

func fecUDP() transport.LegRef {
	return transport.LegRef{Kind: transport.KindUDP, EndpointID: "ep", RemoteAddr: &fecAddr{addr: "127.0.0.1:9000"}}
}

func fecIPv4(id byte, totalLen int) []byte {
	if totalLen < 21 {
		totalLen = 21
	}
	p := make([]byte, totalLen)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(totalLen))
	p[20] = id
	return p
}

func waitForFECHello(t *testing.T, s *send.Send, laneID uint8) protocol.Frame {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil && f.Type == protocol.TypeHELLO && f.LaneID == laneID {
				return f
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("timed out waiting for HELLO")
	return protocol.Frame{}
}

func drainFECFrames(t *testing.T, s *send.Send) []protocol.Frame {
	t.Helper()
	var frames []protocol.Frame
	for {
		select {
		case payload := <-s.Packets():
			f, err := protocol.Decode(payload.Packet.Payload)
			payload.Packet.Release()
			if err == nil {
				frames = append(frames, f)
			}
		default:
			return frames
		}
	}
}

func TestHelloAckWithoutFECDoesNotEnableRepair(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		SessionManager: sessions,
		BootstrapLanes: []send.BootstrapLane{{LaneID: 1, Weight: 100, Leg: fecUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})
	s.EnableFEC()
	handler := runtime.NewRecvHandler(s, sessions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	helloFrame := waitForFECHello(t, s, 1)
	hello := helloFrame.Body.(protocol.HelloBody)
	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: helloFrame.SessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      hello.Nonce,
			Accepted:   1,
			Caps:       0,
			FECProfile: protocol.FECProfileOff,
		},
	}
	if err := handler.OnHelloAck(ctx, fecUDP(), ack); err != nil {
		t.Fatalf("OnHelloAck: %v", err)
	}

	for i := 0; i < 4; i++ {
		pkt := packetbuf.Acquire(40)
		pkt.Payload = fecIPv4(byte(i+1), 40)
		if err := s.Write(ctx, pkt); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	for _, frame := range drainFECFrames(t, s) {
		if frame.Type == protocol.TypeREPAIR {
			t.Fatal("REPAIR produced without negotiated FEC")
		}
	}
}

func TestHelloAckFECIgnoredWhenLocalFECDisabled(t *testing.T) {
	sessions := &sessionpkg.Manager{}
	s := send.New(send.Config{
		SessionManager: sessions,
		BootstrapLanes: []send.BootstrapLane{{LaneID: 1, Weight: 100, Leg: fecUDP()}},
		ProbeInterval:  time.Hour,
		ProbeTimeout:   time.Hour,
	})
	handler := runtime.NewRecvHandler(s, sessions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	helloFrame := waitForFECHello(t, s, 1)
	hello := helloFrame.Body.(protocol.HelloBody)
	if hello.Caps&protocol.CapFEC != 0 {
		t.Fatal("local-disabled HELLO advertised FEC")
	}

	ack := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: helloFrame.SessionID,
		LaneID:    1,
		Body: protocol.HelloAckBody{
			Nonce:      hello.Nonce,
			Accepted:   1,
			Caps:       protocol.CapFEC,
			FECProfile: protocol.FECProfileSLC4Plus1,
		},
	}
	if err := handler.OnHelloAck(ctx, fecUDP(), ack); err != nil {
		t.Fatalf("OnHelloAck: %v", err)
	}

	for i := 0; i < 4; i++ {
		pkt := packetbuf.Acquire(40)
		pkt.Payload = fecIPv4(byte(i+1), 40)
		if err := s.Write(ctx, pkt); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	for _, frame := range drainFECFrames(t, s) {
		if frame.Type == protocol.TypeREPAIR {
			t.Fatal("REPAIR produced even though local FEC was disabled")
		}
	}
}
