package batch

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritevResize(t *testing.T) {
	bufs := [][]byte{
		{1, 2, 3, 4},
		{5, 6, 7, 8, 9, 10, 11},
		{12, 13},
		{14, 15, 16, 17, 18, 19},
		{20},
	}

	testCases := []struct {
		name     string
		consumed int64
		expect   []unix.Iovec
	}{
		{
			name:     "Zero Consumed",
			consumed: 0,
			expect: []unix.Iovec{
				{Base: &bufs[0][0], Len: uint64(len(bufs[0]))},
				{Base: &bufs[1][0], Len: uint64(len(bufs[1]))},
				{Base: &bufs[2][0], Len: uint64(len(bufs[2]))},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed First",
			consumed: 2,
			expect: []unix.Iovec{
				{Base: &bufs[0][2], Len: uint64(2)},
				{Base: &bufs[1][0], Len: uint64(len(bufs[1]))},
				{Base: &bufs[2][0], Len: uint64(len(bufs[2]))},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed First Edge",
			consumed: 4,
			expect: []unix.Iovec{
				{Base: &bufs[1][0], Len: uint64(len(bufs[1]))},
				{Base: &bufs[2][0], Len: uint64(len(bufs[2]))},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed Second",
			consumed: 7,
			expect: []unix.Iovec{
				{Base: &bufs[1][3], Len: uint64(4)},
				{Base: &bufs[2][0], Len: uint64(len(bufs[2]))},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed Second Edge",
			consumed: 11,
			expect: []unix.Iovec{
				{Base: &bufs[2][0], Len: uint64(len(bufs[2]))},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed Third",
			consumed: 12,
			expect: []unix.Iovec{
				{Base: &bufs[2][1], Len: uint64(1)},
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed Third Edge",
			consumed: 13,
			expect: []unix.Iovec{
				{Base: &bufs[3][0], Len: uint64(len(bufs[3]))},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},

		{
			name:     "Consumed Fourth",
			consumed: 17,
			expect: []unix.Iovec{
				{Base: &bufs[3][4], Len: uint64(2)},
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},

		{
			name:     "Consumed Fourth Edge",
			consumed: 19,
			expect: []unix.Iovec{
				{Base: &bufs[4][0], Len: uint64(len(bufs[4]))},
			},
		},
		{
			name:     "Consumed Fifth Edge",
			consumed: 20,
			expect:   []unix.Iovec{},
		},
	}

	f2 := &batchWriter{}
	f2.fillIov(bufs)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := &batchWriter{}
			f.fillIov(bufs)

			f.resizeIov(tc.consumed)

			if !reflect.DeepEqual(tc.expect, f.ioves[f.pos:]) {
				t.Fatalf("unexpected resize result: want: %v got: %v", tc.expect, f.ioves[f.pos:])
			}
		})

		t.Run(fmt.Sprintf("%s Continuously", tc.name), func(t *testing.T) {
			f2.resizeIov(tc.consumed)

			if !reflect.DeepEqual(tc.expect, f2.ioves[f2.pos:]) {
				t.Fatalf("unexpected resize result: want: %v got: %v", tc.expect, f2.ioves[f2.pos:])
			}
		})

	}
}

func TestWritevRandom(t *testing.T) {
	// 64K * 1024
	bufs := make([][]byte, 1024)

	for i := range bufs {
		size := rand.IntN(65536) + 1
		bufs[i] = make([]byte, size)
	}

	selectedIdx := rand.IntN(len(bufs))

	var consumed int64

	for i := 0; i < selectedIdx; i++ {
		consumed += int64(len(bufs[i]))
	}
	offset := rand.IntN(len(bufs[selectedIdx]))

	consumed += int64(offset)

	f := &batchWriter{}
	f.fillIov(bufs)
	f.resizeIov(consumed)

	if f.pos != selectedIdx {
		t.Errorf("unexpected pos: want: %d got: %d", f.pos, selectedIdx)
		return
	}

	if f.ioves[f.pos:][0].Base != &bufs[selectedIdx][offset] {
		t.Errorf("unexpected ptr")
		return
	}

	if f.ioves[f.pos:][0].Len != uint64(len(bufs[selectedIdx])-offset) {
		t.Errorf("unexpected size: want: %d got: %d", len(bufs[selectedIdx])-offset, f.ioves[f.pos:][0].Len)
	}

}
