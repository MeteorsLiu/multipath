package protocol

import (
	"errors"
	"reflect"
	"testing"
)

func TestTypedFramesRoundTrip(t *testing.T) {
	tests := []Frame{
		{
			Type:      TypeHELLO,
			SessionID: 11,
			LaneID:    1,
			Body: HelloBody{
				Nonce:      9,
				Caps:       3,
				FECProfile: FECProfileSLC4Plus1,
			},
		},
		{
			Type:      TypeHELLOACK,
			SessionID: 11,
			LaneID:    1,
			Body: HelloAckBody{
				Nonce:      9,
				Accepted:   1,
				Caps:       CapTCPFallback,
				FECProfile: FECProfileOff,
			},
		},
		{
			Type:      TypePING,
			SessionID: 11,
			LaneID:    1,
			Body:      PingBody{PingID: 7, TimeMS: 12345},
		},
		{
			Type:      TypePONG,
			SessionID: 11,
			LaneID:    1,
			Body:      PingBody{PingID: 8, TimeMS: 12346},
		},
		{
			Type:      TypeDATA,
			SessionID: 11,
			LaneID:    1,
			Body:      DataBody{PacketID: 0x01020304, Packet: []byte("packet")},
		},
		{
			Type:      TypeREPAIR,
			SessionID: 11,
			LaneID:    1,
			Body:      RepairBody{BasePacketID: 10, Key: 7, SourceSpan: 4, Symbol: []byte("repair")},
		},
		{
			Type:      TypeCLOSE,
			SessionID: 11,
			LaneID:    SessionControlLaneID,
			Body:      CloseBody{Scope: CloseScopeSession, Reason: 9},
		},
		{
			Type:      TypeBandwidthProbe,
			SessionID: 11,
			LaneID:    1,
			Body:      BandwidthProbeBody{ProbeID: 99, Seq: 2, Count: 4, SendMS: 12347, Payload: []byte("probe")},
		},
		{
			Type:      TypeBandwidthProbeAck,
			SessionID: 11,
			LaneID:    1,
			Body:      BandwidthProbeAckBody{ProbeID: 99, Count: 4, Received: 0x0d, FirstRXMS: 12350, LastRXMS: 12355},
		},
	}

	for _, frame := range tests {
		encoded, err := Encode(frame, nil)
		if err != nil {
			t.Fatalf("Encode type %d failed: %v", frame.Type, err)
		}
		got, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode type %d failed: %v", frame.Type, err)
		}
		assertFrameEqual(t, got, frame)
	}
}

func TestBodyTooShort(t *testing.T) {
	for _, frameType := range []FrameType{
		TypeHELLO,
		TypeHELLOACK,
		TypePING,
		TypePONG,
		TypeDATA,
		TypeREPAIR,
		TypeCLOSE,
		TypeBandwidthProbe,
		TypeBandwidthProbeAck,
	} {
		frame := make([]byte, headerSize+1)
		frame[0] = Version<<4 | uint8(frameType)
		if _, err := Decode(frame); !errors.Is(err, ErrBodyTooShort) {
			t.Fatalf("Decode type %d err = %v, want ErrBodyTooShort", frameType, err)
		}
	}
}

func assertFrameEqual(t *testing.T, got Frame, want Frame) {
	t.Helper()
	if got.Type != want.Type || got.SessionID != want.SessionID || got.LaneID != want.LaneID {
		t.Fatalf("route = type %d session %d lane %d, want type %d session %d lane %d",
			got.Type, got.SessionID, got.LaneID, want.Type, want.SessionID, want.LaneID)
	}
	if !reflect.DeepEqual(got.Body, want.Body) {
		t.Fatalf("body = %#v, want %#v", got.Body, want.Body)
	}
}
