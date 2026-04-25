package fec

import "testing"

func BenchmarkCodecEncode4Plus1(b *testing.B) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		b.Fatal(err)
	}
	shards := [][]byte{
		make([]byte, 1436),
		make([]byte, 1436),
		make([]byte, 1436),
		make([]byte, 1436),
		make([]byte, 0, 1436),
	}

	b.ReportAllocs()
	b.SetBytes(int64(4 * 1436))
	for i := 0; i < b.N; i++ {
		shards[4] = shards[4][:0]
		if err := codec.Encode(shards, uint16(i)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCodecReconstruct4Plus1(b *testing.B) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		b.Fatal(err)
	}
	source := [][]byte{
		make([]byte, 1436),
		make([]byte, 1436),
		make([]byte, 1436),
		make([]byte, 1436),
		nil,
	}
	if err := codec.Encode(source, 7); err != nil {
		b.Fatal(err)
	}
	repair := source[4]
	recovered := make([]byte, 0, 1436)
	shards := [][]byte{
		source[0],
		recovered,
		source[2],
		source[3],
		repair,
	}

	b.ReportAllocs()
	b.SetBytes(int64(4 * 1436))
	for i := 0; i < b.N; i++ {
		shards[1] = recovered[:0]
		if err := codec.Reconstruct(shards, 7); err != nil {
			b.Fatal(err)
		}
		recovered = shards[1]
	}
}
