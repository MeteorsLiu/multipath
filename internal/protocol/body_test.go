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
			Body: BandwidthProbeBody{
				TrainID:             77,
				ProbeID:             99,
				Seq:                 2,
				Count:               4,
				SendMS:              12347,
				TrainBytesTotal:     1000,
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

func TestBandwidthProbeRejectsInvalidTrainBudget(t *testing.T) {
	tests := []BandwidthProbeBody{
		{TrainID: 1, ProbeID: 1, Seq: 0, Count: 1, SendMS: 1, TrainBytesTotal: 0, TrainBytesRemaining: 0},
		{TrainID: 1, ProbeID: 1, Seq: 0, Count: 1, SendMS: 1, TrainBytesTotal: 100, TrainBytesRemaining: 101},
	}
	for _, body := range tests {
		_, err := Encode(Frame{Type: TypeBandwidthProbe, SessionID: 1, LaneID: 1, Body: body}, nil)
		if !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("Encode(%+v) err = %v, want ErrInvalidFrame", body, err)
		}
	}
}

func TestBandwidthProbeDecodeRejectsInvalidTrainBudget(t *testing.T) {
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
			TrainBytesTotal:     100,
			TrainBytesRemaining: 100,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Encode valid probe failed: %v", err)
	}
	binary.BigEndian.PutUint64(encoded[10+36:10+44], 101)
	if _, err := Decode(encoded); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode invalid remaining err = %v, want ErrInvalidFrame", err)
	}
}

func TestLinkStatusRoundTrip(t *testing.T) {
	frame := Frame{
		Type:      TypeLinkStatus,
		SessionID: 11,
		LaneID:    1,
		Body: LinkStatusBody{
			LegKind:      LinkStatusLegUDP,
			Reason:       LinkStatusReasonLimited,
			DeliveredBps: 2_000_000,
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
		{LegKind: 0, Reason: LinkStatusReasonLimited, DeliveredBps: 1},
		{LegKind: LinkStatusLegUDP, Reason: 0, DeliveredBps: 1},
		{LegKind: LinkStatusLegTCP + 1, Reason: LinkStatusReasonLimited, DeliveredBps: 1},
		{LegKind: LinkStatusLegUDP, Reason: LinkStatusReasonLimited + 1, DeliveredBps: 1},
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
