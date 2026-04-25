package packetbuf

import "testing"

func BenchmarkAcquireRelease1500(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		packet := Acquire(1500)
		packet.SetLen(1200)
		packet.Release()
	}
}

func BenchmarkAcquireRelease64K(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		packet := Acquire(64 * 1024)
		packet.SetLen(1500)
		packet.Release()
	}
}
