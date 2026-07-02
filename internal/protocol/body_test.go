package protocol

import (
	"encoding/binary"
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
			Body:      RepairBody{BasePacketID: 10, Key: 7, SourceSpan: 4, RepairCount: 1, Symbol: []byte("repair")},
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
			Body: BandwidthProbeBody{
				TrainID:             77,
				ProbeID:             99,
				Seq:                 2,
				Count:               4,
				SendMS:              12347,
				TargetBps:           200_000_000,
				TrainBytesRemaining: 250,
				Payload:             []byte("probe"),
			},
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

func TestRepairBodyPacksSourceSpanAndRepairCount(t *testing.T) {
	body := RepairBody{BasePacketID: 10, Key: 7, SourceSpan: 4, Symbol: []byte("repair")}
	setRepairCountForTest(t, &body, 3)
	encoded, err := Encode(Frame{Type: TypeREPAIR, SessionID: 11, LaneID: 1, Body: body}, nil)
	if err != nil {
		t.Fatalf("Encode REPAIR failed: %v", err)
	}
	if got, want := encoded[headerSize+6], uint8(0x14); got != want {
		t.Fatalf("packed source_span byte = %#x, want %#x", got, want)
	}

	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode REPAIR failed: %v", err)
	}
	repair := got.Body.(RepairBody)
	if repair.SourceSpan != 4 {
		t.Fatalf("decoded source span = %d, want 4", repair.SourceSpan)
	}
	if got := repairCountForTest(t, repair); got != 3 {
		t.Fatalf("decoded repair count = %d, want 3", got)
	}
}

func TestRepairBodyRejectsReservedSourceSpanBits(t *testing.T) {
	encoded, err := Encode(Frame{
		Type:      TypeREPAIR,
		SessionID: 11,
		LaneID:    1,
		Body:      RepairBody{BasePacketID: 10, Key: 7, SourceSpan: 4, Symbol: []byte("repair")},
	}, nil)
	if err != nil {
		t.Fatalf("Encode REPAIR failed: %v", err)
	}
	encoded[headerSize+6] = 0x24
	if _, err := Decode(encoded); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode reserved repair source_span err = %v, want ErrInvalidFrame", err)
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

func setRepairCountForTest(t *testing.T, body *RepairBody, repairCount uint8) {
	t.Helper()
	field := reflect.ValueOf(body).Elem().FieldByName("RepairCount")
	if !field.IsValid() {
		t.Fatal("RepairBody missing RepairCount field")
	}
	field.SetUint(uint64(repairCount))
}

func repairCountForTest(t *testing.T, body RepairBody) uint8 {
	t.Helper()
	field := reflect.ValueOf(body).FieldByName("RepairCount")
	if !field.IsValid() {
		t.Fatal("RepairBody missing RepairCount field")
	}
	return uint8(field.Uint())
}

func TestBandwidthProbeRejectsInvalidRoundShape(t *testing.T) {
	tests := []BandwidthProbeBody{
		{TrainID: 1, ProbeID: 1, Seq: 0, Count: 0, SendMS: 1, TargetBps: 200_000_000, TrainBytesRemaining: 0},
		{TrainID: 1, ProbeID: 1, Seq: 64, Count: 64, SendMS: 1, TargetBps: 200_000_000, TrainBytesRemaining: 0},
	}
	for _, body := range tests {
		_, err := Encode(Frame{Type: TypeBandwidthProbe, SessionID: 1, LaneID: 1, Body: body}, nil)
		if !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("Encode(%+v) err = %v, want ErrInvalidFrame", body, err)
		}
	}
}

func TestBandwidthProbeTargetBpsUsesFormerTotalField(t *testing.T) {
	encoded, err := Encode(Frame{
		Type:      TypeBandwidthProbe,
		SessionID: 1,
		LaneID:    1,
		Body: BandwidthProbeBody{
			TrainID:             1,
			ProbeID:             1,
			Seq:                 0,
			Count:               1,
			SendMS:              1,
			TargetBps:           123_456_789,
			TrainBytesRemaining: 987,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode valid probe failed: %v", err)
	}
	if got := binary.BigEndian.Uint64(encoded[10+28 : 10+36]); got != 123_456_789 {
		t.Fatalf("encoded target_bps = %d, want 123456789", got)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode probe failed: %v", err)
	}
	body := decoded.Body.(BandwidthProbeBody)
	if body.TargetBps != 123_456_789 || body.TrainBytesRemaining != 987 {
		t.Fatalf("decoded probe = %+v, want target_bps 123456789 remaining 987", body)
	}
}

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

func TestCapLinkStatusIsSupportedWithFEC(t *testing.T) {
	if CapLinkStatus == 0 || CapLinkStatus == CapFEC || CapLinkStatus == CapTCPFallback {
		t.Fatalf("CapLinkStatus = %#x overlaps existing caps", CapLinkStatus)
	}
	if SupportedCaps&(CapFEC|CapLinkStatus) != (CapFEC | CapLinkStatus) {
		t.Fatalf("SupportedCaps = %#x, want FEC and LinkStatus", SupportedCaps)
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
