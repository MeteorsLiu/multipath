package protocol

import "testing"

func BenchmarkEncodeDATA(b *testing.B) {
	packet := make([]byte, 1436)
	frame := Frame{
		Type:      TypeDATA,
		SessionID: 1,
		LaneID:    2,
		Body:      DataBody{GroupID: 3, SourceIndex: 1, Packet: packet},
	}
	dst := make([]byte, 0, headerSize+4+len(packet))

	b.ReportAllocs()
	b.SetBytes(int64(len(packet)))
	for i := 0; i < b.N; i++ {
		var err error
		dst = dst[:0]
		dst, err = Encode(frame, dst)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeREPAIR(b *testing.B) {
	symbol := make([]byte, 1434)
	frame := Frame{
		Type:      TypeREPAIR,
		SessionID: 1,
		LaneID:    2,
		Body: RepairBody{
			GroupID:    3,
			Key:        4,
			SourceSpan: 4,
			Symbol:     symbol,
		},
	}
	dst := make([]byte, 0, headerSize+7+len(symbol))

	b.ReportAllocs()
	b.SetBytes(int64(len(symbol)))
	for i := 0; i < b.N; i++ {
		var err error
		dst = dst[:0]
		dst, err = Encode(frame, dst)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeDATA(b *testing.B) {
	packet := make([]byte, 1436)
	encoded, err := Encode(Frame{
		Type:      TypeDATA,
		SessionID: 1,
		LaneID:    2,
		Body:      DataBody{GroupID: 3, SourceIndex: 1, Packet: packet},
	}, nil)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(packet)))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(encoded); err != nil {
			b.Fatal(err)
		}
	}
}
