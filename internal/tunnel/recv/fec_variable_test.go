package recv

import (
	"context"
	"testing"

	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/session"
)

func TestRecvRecoversVariableSourceSpan(t *testing.T) {
	var manager session.Manager
	if _, ok := manager.Create(99); !ok {
		t.Fatal("Create session failed")
	}
	out := New(Config{SessionManager: &manager})

	packet100 := recvIPv4TestPacket(20)
	recovered := recvIPv4TestPacket(28)
	dataFrame := encodedTestFrame(t, protocol.Frame{
		Type:      protocol.TypeDATA,
		SessionID: 99,
		LaneID:    1,
		Body:      protocol.DataBody{PacketID: 100, Packet: packet100},
	})
	if err := out.Write(context.Background(), dataFrame); err != nil {
		t.Fatalf("Write DATA: %v", err)
	}

	codec, err := fecpkg.NewCodec(2, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	shards := [][]byte{packet100, recovered, nil}
	if err := codec.Encode(shards, 7); err != nil {
		t.Fatalf("Encode repair: %v", err)
	}
	repairFrame := encodedTestFrame(t, protocol.Frame{
		Type:      protocol.TypeREPAIR,
		SessionID: 99,
		LaneID:    1,
		Body: protocol.RepairBody{
			BasePacketID: 100,
			Key:          7,
			SourceSpan:   2,
			Symbol:       shards[2],
		},
	})
	if err := out.Write(context.Background(), repairFrame); err != nil {
		t.Fatalf("Write REPAIR: %v", err)
	}

	first := readRecvPacket(t, out)
	defer first.Release()
	second := readRecvPacket(t, out)
	defer second.Release()
	if string(first.Payload) != string(packet100) {
		t.Fatalf("first payload = %v, want packet100", first.Payload)
	}
	if string(second.Payload) != string(recovered) {
		t.Fatalf("second payload = %v, want recovered", second.Payload)
	}
}

func recvIPv4TestPacket(totalLen int) []byte {
	packet := make([]byte, totalLen)
	packet[0] = 0x45
	packet[2] = byte(totalLen >> 8)
	packet[3] = byte(totalLen)
	return packet
}
