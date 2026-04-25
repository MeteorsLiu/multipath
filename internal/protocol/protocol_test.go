package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestCodecRoundTrip(t *testing.T) {
	frame := Frame{
		Type:      TypeDATA,
		SessionID: 0x0102030405060708,
		LaneID:    7,
		Body:      DataBody{PacketID: 9, Packet: []byte("packet")},
	}

	encoded, err := Encode(frame, []byte{0xaa})
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if encoded[0] != 0xaa {
		t.Fatalf("prefix byte changed: got %#x", encoded[0])
	}

	got, err := Decode(encoded[1:])
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if got.Version != Version {
		t.Fatalf("Version = %d, want %d", got.Version, Version)
	}
	if got.Type != frame.Type {
		t.Fatalf("Type = %d, want %d", got.Type, frame.Type)
	}
	if got.SessionID != frame.SessionID {
		t.Fatalf("SessionID = %#x, want %#x", got.SessionID, frame.SessionID)
	}
	if got.LaneID != frame.LaneID {
		t.Fatalf("LaneID = %d, want %d", got.LaneID, frame.LaneID)
	}
	gotBody, ok := got.Body.(DataBody)
	if !ok {
		t.Fatalf("body type = %T, want DataBody", got.Body)
	}
	wantBody := frame.Body.(DataBody)
	if gotBody.PacketID != wantBody.PacketID {
		t.Fatalf("PacketID = %d, want %d", gotBody.PacketID, wantBody.PacketID)
	}
	if !bytes.Equal(gotBody.Packet, wantBody.Packet) {
		t.Fatalf("Packet = %q, want %q", gotBody.Packet, wantBody.Packet)
	}
}

func TestCodecRejectsInvalidFrames(t *testing.T) {
	if _, err := Decode(make([]byte, headerSize-1)); !errors.Is(err, ErrFrameTooShort) {
		t.Fatalf("Decode short err = %v, want ErrFrameTooShort", err)
	}
	if _, err := Decode(make([]byte, headerSize)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode zero vt err = %v, want ErrInvalidFrame", err)
	}
	unknownVersion := make([]byte, headerSize)
	unknownVersion[0] = 2<<4 | uint8(TypeDATA)
	if _, err := Decode(unknownVersion); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode unknown version err = %v, want ErrInvalidFrame", err)
	}
	unknownType := make([]byte, headerSize)
	unknownType[0] = Version<<4 | 0x0f
	if _, err := Decode(unknownType); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Decode unknown type err = %v, want ErrInvalidFrame", err)
	}
	if _, err := Encode(Frame{Version: 2, Type: TypeDATA}, nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Encode invalid version err = %v, want ErrInvalidFrame", err)
	}
	if _, err := Encode(Frame{Type: TypeCLOSE + 1}, nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Encode invalid type err = %v, want ErrInvalidFrame", err)
	}
	if _, err := Encode(Frame{Type: TypeDATA, Body: HelloBody{}}, nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("Encode mismatched body err = %v, want ErrInvalidFrame", err)
	}
}
