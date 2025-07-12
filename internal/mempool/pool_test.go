package mempool

import (
	"encoding/binary"
	"testing"
)

func BenchmarkGet(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := Get(32768)
		Put(buf)
	}
}

func bePut(buf []byte, i int) {
	binary.LittleEndian.PutUint16(buf[0:2], uint16(i))
}

func BenchmarkNotEscape(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		buf := make([]byte, 32768)
		bePut(buf, i)
	}

}

var bf []byte

func BenchmarkEscape(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		bf = make([]byte, 32768)
		_ = bf
	}

}

func TestReset(t *testing.T) {
	t1 := Get(16383)

	for i := 0; i < 2; i++ {
		t1.B[i] = byte(i)
	}

	t.Log(t1)

	Put(t1)

	t2 := Get(16383)
	t.Log(t2)
	t2.Reset()
	t.Log(t2)
}
