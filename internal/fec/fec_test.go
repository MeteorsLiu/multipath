package fec

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/klauspost/reedsolomon"
)

func TestTinyMT32SeedOneMatchesRFC8682(t *testing.T) {
	rng := newTinyMT32(1)
	want := []uint32{
		2545341989,
		981918433,
		3715302833,
		2387538352,
		3591001365,
	}
	for i, value := range want {
		if got := rng.generate(); got != value {
			t.Fatalf("generate %d = %d, want %d", i, got, value)
		}
	}
}

func TestCodecReconstructsOneMissingShard(t *testing.T) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}

	shards := [][]byte{
		[]byte("abcd"),
		[]byte("ef"),
		[]byte("ghij"),
		[]byte("klm"),
		nil,
	}
	if err := codec.Encode(shards, 7); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	repair := append([]byte(nil), shards[4]...)

	recovered := [][]byte{
		[]byte("abcd"),
		nil,
		[]byte("ghij"),
		[]byte("klm"),
		repair,
	}
	if err := codec.Reconstruct(recovered, 7); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if !bytes.Equal(recovered[1], []byte{'e', 'f', 0, 0}) {
		t.Fatalf("recovered = %v, want ef with virtual zero padding", recovered[1])
	}
}

func TestCodecReconstructsEachMissingShard(t *testing.T) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	source := [][]byte{
		[]byte("aaaa"),
		[]byte("bbb"),
		[]byte("cc"),
		[]byte("d"),
		nil,
	}
	if err := codec.Encode(source, 99); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	repair := append([]byte(nil), source[4]...)

	for missing := 0; missing < 4; missing++ {
		shards := make([][]byte, 5)
		for i := 0; i < 4; i++ {
			if i == missing {
				continue
			}
			shards[i] = source[i]
		}
		shards[4] = repair

		if err := codec.Reconstruct(shards, 99); err != nil {
			t.Fatalf("Reconstruct missing %d: %v", missing, err)
		}
		want := append([]byte(nil), source[missing]...)
		for len(want) < len(repair) {
			want = append(want, 0)
		}
		if !bytes.Equal(shards[missing], want) {
			t.Fatalf("missing %d recovered = %v, want %v", missing, shards[missing], want)
		}
	}
}

func TestCodecMatchesReedSolomonCustomMatrix(t *testing.T) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}

	tests := []struct {
		key    uint16
		shards [][]byte
	}{
		{
			key: 7,
			shards: [][]byte{
				[]byte("abcd"),
				[]byte("ef"),
				[]byte("ghij"),
				[]byte("klm"),
			},
		},
		{
			key: 0,
			shards: [][]byte{
				[]byte{0, 1, 2, 3, 4, 5},
				[]byte{6, 7, 8},
				[]byte{9, 10, 11, 12},
				[]byte{13},
			},
		},
		{
			key: 65535,
			shards: [][]byte{
				bytes.Repeat([]byte{1}, 1450),
				bytes.Repeat([]byte{2}, 1441),
				bytes.Repeat([]byte{3}, 1430),
				bytes.Repeat([]byte{4}, 1400),
			},
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("key_%d", tt.key), func(t *testing.T) {
			got := cloneDataShards(tt.shards)
			got = append(got, nil)
			if err := codec.Encode(got, tt.key); err != nil {
				t.Fatalf("Encode: %v", err)
			}

			rsWork, encoder := reedSolomonWork(t, tt.shards, tt.key)
			if err := encoder.Encode(rsWork); err != nil {
				t.Fatalf("reedsolomon Encode: %v", err)
			}
			if !bytes.Equal(got[4], rsWork[4]) {
				t.Fatalf("repair mismatch")
			}

			for missing := 0; missing < 4; missing++ {
				recovered := cloneDataShards(tt.shards)
				recovered[missing] = nil
				recovered = append(recovered, append([]byte(nil), got[4]...))
				if err := codec.Reconstruct(recovered, tt.key); err != nil {
					t.Fatalf("Reconstruct missing %d: %v", missing, err)
				}

				rsRecover, encoder := reedSolomonWork(t, tt.shards, tt.key)
				if err := encoder.Encode(rsRecover); err != nil {
					t.Fatalf("reedsolomon Encode for missing %d: %v", missing, err)
				}
				rsRecover[missing] = nil
				if err := encoder.ReconstructData(rsRecover); err != nil {
					t.Fatalf("reedsolomon ReconstructData missing %d: %v", missing, err)
				}
				if !bytes.Equal(recovered[missing], rsRecover[missing]) {
					t.Fatalf("missing %d recovered mismatch", missing)
				}
			}
		})
	}
}

func TestCodecRejectsUnrecoverableShards(t *testing.T) {
	codec, err := NewCodec(4, 1)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}

	shards := [][]byte{
		[]byte("abcd"),
		nil,
		nil,
		[]byte("klm"),
		[]byte("repair"),
	}
	if err := codec.Reconstruct(shards, 7); !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("Reconstruct err = %v, want ErrUnrecoverable", err)
	}
}

func TestNewCodecRejectsUnsupportedRepairCount(t *testing.T) {
	if _, err := NewCodec(4, 2); !errors.Is(err, ErrInvalidShardConfig) {
		t.Fatalf("NewCodec err = %v, want ErrInvalidShardConfig", err)
	}
}

func cloneDataShards(shards [][]byte) [][]byte {
	out := make([][]byte, len(shards))
	for i := range shards {
		out[i] = append([]byte(nil), shards[i]...)
	}
	return out
}

func reedSolomonWork(t *testing.T, shards [][]byte, key uint16) ([][]byte, reedsolomon.Encoder) {
	t.Helper()

	size := 0
	for _, shard := range shards {
		if len(shard) > size {
			size = len(shard)
		}
	}
	work := make([][]byte, len(shards)+1)
	for i := range shards {
		work[i] = make([]byte, size)
		copy(work[i], shards[i])
	}
	work[len(shards)] = make([]byte, size)

	coeffs := make([]byte, len(shards))
	fillCodingCoefficients(key, coeffs)
	encoder, err := reedsolomon.New(
		len(shards),
		1,
		reedsolomon.WithCustomMatrix([][]byte{coeffs}),
		reedsolomon.WithMaxGoroutines(1),
	)
	if err != nil {
		t.Fatalf("reedsolomon.New: %v", err)
	}
	return work, encoder
}
