package mempool

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
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

	t.Log(t1)

	Put(t1)

	t2 := Get(16383)
	t.Log(t2)
	t2.Reset()
	t.Log(t2)
}

// TestBuffer_ToBuffer tests the ToBuffer constructor
func TestBuffer_ToBuffer(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("ToBuffer failed: expected %v, got %v", data, buf.Bytes())
	}

	if buf.Len() != len(data) {
		t.Errorf("ToBuffer length failed: expected %d, got %d", len(data), buf.Len())
	}
}

// TestBuffer_Read tests the Read method
func TestBuffer_Read(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	// Test reading full buffer
	readBuf := make([]byte, len(data))
	n, err := buf.Read(readBuf)
	if err != nil {
		t.Errorf("Read failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Read count failed: expected %d, got %d", len(data), n)
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("Read data failed: expected %v, got %v", data, readBuf)
	}

	// Test reading with smaller buffer
	buf = ToBuffer(data)
	smallBuf := make([]byte, 5)
	n, err = buf.Read(smallBuf)
	if err != nil {
		t.Errorf("Read with small buffer failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Read count with small buffer failed: expected 5, got %d", n)
	}
	if !bytes.Equal(smallBuf, data[:5]) {
		t.Errorf("Read data with small buffer failed: expected %v, got %v", data[:5], smallBuf)
	}

	// Test reading after consuming
	buf = ToBuffer(data)
	buf.Consume(5)
	readBuf = make([]byte, 10)
	n, err = buf.Read(readBuf)
	if err != nil {
		t.Errorf("Read after consume failed: %v", err)
	}
	expected := data[5:]
	if n != len(expected) {
		t.Errorf("Read count after consume failed: expected %d, got %d", len(expected), n)
	}
	if !bytes.Equal(readBuf[:n], expected) {
		t.Errorf("Read data after consume failed: expected %v, got %v", expected, readBuf[:n])
	}
}

// TestBuffer_Write tests the Write method
func TestBuffer_Write(t *testing.T) {
	buf := Get(20)
	defer Put(buf)

	data := []byte("hello")
	n, err := buf.Write(data)
	if err != nil {
		t.Errorf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write count failed: expected %d, got %d", len(data), n)
	}

	// Check that cursor moved
	if buf.ConsumedBytes() != len(data) {
		t.Errorf("Write cursor failed: expected %d, got %d", len(data), buf.ConsumedBytes())
	}

	// Write more data
	data2 := []byte(" world")
	n, err = buf.Write(data2)
	if err != nil {
		t.Errorf("Second write failed: %v", err)
	}
	if n != len(data2) {
		t.Errorf("Second write count failed: expected %d, got %d", len(data2), n)
	}

	// Reset cursor to read all data
	buf.OffsetTo(0)
	result := buf.Bytes()
	expected := []byte("hello world")
	if !bytes.Equal(result[:len(expected)], expected) {
		t.Errorf("Write result failed: expected %v, got %v", expected, result[:len(expected)])
	}
}

// TestBuffer_ReadFrom tests the ReadFrom method
func TestBuffer_ReadFrom(t *testing.T) {
	buf := Get(20)
	defer Put(buf)

	data := "hello world"
	reader := strings.NewReader(data)

	n, err := buf.ReadFrom(reader)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Errorf("ReadFrom failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("ReadFrom count failed: expected %d, got %d", len(data), n)
	}

	// Check data was read correctly
	if !bytes.Equal(buf.Bytes()[:len(data)], []byte(data)) {
		t.Errorf("ReadFrom data failed: expected %v, got %v", []byte(data), buf.Bytes()[:len(data)])
	}
}

// TestBuffer_Bytes tests the Bytes method
func TestBuffer_Bytes(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	result := buf.Bytes()
	if !bytes.Equal(result, data) {
		t.Errorf("Bytes failed: expected %v, got %v", data, result)
	}

	// Test after consuming
	buf.Consume(5)
	result = buf.Bytes()
	expected := data[5:]
	if !bytes.Equal(result, expected) {
		t.Errorf("Bytes after consume failed: expected %v, got %v", expected, result)
	}
}

// TestBuffer_OffsetTo tests the OffsetTo method
func TestBuffer_OffsetTo(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	buf.OffsetTo(5)
	if buf.ConsumedBytes() != 5 {
		t.Errorf("OffsetTo failed: expected 5, got %d", buf.ConsumedBytes())
	}

	result := buf.Bytes()
	expected := data[5:]
	if !bytes.Equal(result, expected) {
		t.Errorf("OffsetTo bytes failed: expected %v, got %v", expected, result)
	}
}

// TestBuffer_SetLen tests the SetLen method
func TestBuffer_SetLen(t *testing.T) {
	buf := Get(20)
	defer Put(buf)

	buf.SetLen(10)
	if buf.Len() != 10 {
		t.Errorf("SetLen failed: expected 10, got %d", buf.Len())
	}

	buf.SetLen(5)
	if buf.Len() != 5 {
		t.Errorf("SetLen shrink failed: expected 5, got %d", buf.Len())
	}
}

// TestBuffer_Cap tests the Cap method
func TestBuffer_Cap(t *testing.T) {
	buf := Get(1024)
	defer Put(buf)

	if buf.Cap() != 1024 {
		t.Errorf("Cap failed: expected 1024, got %d", buf.Cap())
	}
}

// TestBuffer_Len tests the Len method
func TestBuffer_Len(t *testing.T) {
	// Test nil buffer
	var buf *Buffer
	if buf.Len() != 0 {
		t.Errorf("Nil buffer Len failed: expected 0, got %d", buf.Len())
	}

	// Test normal buffer
	data := []byte("hello")
	buf = ToBuffer(data)
	if buf.Len() != len(data) {
		t.Errorf("Len failed: expected %d, got %d", len(data), buf.Len())
	}

	// Test after consuming
	buf.Consume(2)
	if buf.Len() != len(data)-2 {
		t.Errorf("Len after consume failed: expected %d, got %d", len(data)-2, buf.Len())
	}
}

// TestBuffer_Consume tests the Consume method
func TestBuffer_Consume(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	buf.Consume(5)
	if buf.ConsumedBytes() != 5 {
		t.Errorf("Consume failed: expected 5, got %d", buf.ConsumedBytes())
	}

	result := buf.Bytes()
	expected := data[5:]
	if !bytes.Equal(result, expected) {
		t.Errorf("Consume bytes failed: expected %v, got %v", expected, result)
	}
}

// TestBuffer_GrowTo tests the GrowTo method
func TestBuffer_GrowTo(t *testing.T) {
	buf := Get(10)
	data := []byte("hello")
	buf.Write(data)
	buf.OffsetTo(0) // Reset position to read all data

	// Test growing to larger size
	buf.GrowTo(20)
	if buf.Cap() < 20 {
		t.Errorf("GrowTo failed: expected capacity >= 20, got %d", buf.Cap())
	}

	// Check data is preserved
	result := buf.Bytes()
	if !bytes.Equal(result[:len(data)], data) {
		t.Errorf("GrowTo data preservation failed: expected %v, got %v", data, result[:len(data)])
	}

	// Test growing to smaller size (should not change)
	buf2 := Get(20)
	defer Put(buf2)
	originalCap := buf2.Cap()
	buf2.GrowTo(10)
	if buf2.Cap() != originalCap {
		t.Errorf("GrowTo smaller failed: capacity should not change, expected %d, got %d", originalCap, buf2.Cap())
	}
}

// TestBuffer_ConsumedBytes tests the ConsumedBytes method
func TestBuffer_ConsumedBytes(t *testing.T) {
	data := []byte("hello world")
	buf := ToBuffer(data)

	if buf.ConsumedBytes() != 0 {
		t.Errorf("Initial ConsumedBytes failed: expected 0, got %d", buf.ConsumedBytes())
	}

	buf.Consume(5)
	if buf.ConsumedBytes() != 5 {
		t.Errorf("ConsumedBytes after consume failed: expected 5, got %d", buf.ConsumedBytes())
	}

	buf.OffsetTo(8)
	if buf.ConsumedBytes() != 8 {
		t.Errorf("ConsumedBytes after offset failed: expected 8, got %d", buf.ConsumedBytes())
	}
}

// TestBuffer_Reset tests the Reset method
func TestBuffer_Reset(t *testing.T) {
	buf := Get(20)
	defer Put(buf)

	// Write some data
	data := []byte("hello world")
	buf.Write(data)
	buf.OffsetTo(0)

	// Reset should clear the data
	buf.Reset()

	// Check that data is cleared (all zeros)
	result := buf.Bytes()
	for i, b := range result {
		if b != 0 {
			t.Errorf("Reset failed: byte at position %d should be 0, got %d", i, b)
			break
		}
	}
}

// TestAllocator tests the Allocator directly
func TestAllocator(t *testing.T) {
	alloc := NewAllocator()

	// Test getting buffers of various sizes
	sizes := []int{1, 2, 3, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536}

	for _, size := range sizes {
		buf := alloc.Get(size)
		if buf == nil {
			t.Errorf("Allocator.Get(%d) returned nil", size)
			continue
		}

		if buf.Len() != size {
			t.Errorf("Allocator.Get(%d) length wrong: expected %d, got %d", size, size, buf.Len())
		}

		// Capacity should be a power of 2 and >= size
		cap := buf.Cap()
		if cap < size {
			t.Errorf("Allocator.Get(%d) capacity too small: expected >= %d, got %d", size, size, cap)
		}

		// Check if capacity is power of 2
		if cap&(cap-1) != 0 {
			t.Errorf("Allocator.Get(%d) capacity not power of 2: got %d", size, cap)
		}

		err := alloc.Put(buf)
		if err != nil {
			t.Errorf("Allocator.Put failed for size %d: %v", size, err)
		}
	}

	// Test invalid Put
	invalidBuf := &Buffer{b: make([]byte, 100)} // Not power of 2
	err := alloc.Put(invalidBuf)
	if err == nil {
		t.Error("Allocator.Put should fail for non-power-of-2 buffer")
	}
}

// TestMsb tests the msb function
func TestMsb(t *testing.T) {
	tests := []struct {
		input    int
		expected uint16
	}{
		{1, 0},
		{2, 1},
		{3, 1},
		{4, 2},
		{7, 2},
		{8, 3},
		{15, 3},
		{16, 4},
		{31, 4},
		{32, 5},
		{63, 5},
		{64, 6},
		{127, 6},
		{128, 7},
		{255, 7},
		{256, 8},
		{511, 8},
		{512, 9},
		{1023, 9},
		{1024, 10},
		{65536, 16},
	}

	for _, test := range tests {
		result := msb(test.input)
		if result != test.expected {
			t.Errorf("msb(%d) failed: expected %d, got %d", test.input, test.expected, result)
		}
	}
}

// TestBuffer_EdgeCases tests edge cases and error conditions
func TestBuffer_EdgeCases(t *testing.T) {
	// Test reading from empty buffer
	buf := Get(10)
	defer Put(buf)
	buf.SetLen(0)

	readBuf := make([]byte, 5)
	n, err := buf.Read(readBuf)
	if err != nil {
		t.Errorf("Read from empty buffer failed: %v", err)
	}
	if n != 0 {
		t.Errorf("Read from empty buffer should return 0, got %d", n)
	}

	// Test writing to buffer with insufficient space
	buf2 := Get(5)
	defer Put(buf2)

	data := []byte("hello world") // longer than buffer
	n, err = buf2.Write(data)
	if err != nil {
		t.Errorf("Write to small buffer failed: %v", err)
	}
	if n != 5 { // should only write what fits
		t.Errorf("Write to small buffer should return 5, got %d", n)
	}

	// Test consuming more than available
	buf3 := ToBuffer([]byte("hello"))
	buf3.Consume(10) // consume more than length
	if buf3.ConsumedBytes() != 10 {
		t.Errorf("Consume should allow consuming more than length, got %d", buf3.ConsumedBytes())
	}
}
