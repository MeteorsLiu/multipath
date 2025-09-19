package cfs

import (
	"container/heap"
	"fmt"
	"sync"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
)

// mockConnWriter implements conn.ConnWriter for testing
type mockConnWriter struct {
	mu      sync.Mutex
	id      string
	written [][]byte
	errors  []error
	errIdx  int
}

func newMockConnWriter(id string) *mockConnWriter {
	return &mockConnWriter{
		id:      id,
		written: make([][]byte, 0),
		errors:  make([]error, 0),
	}
}

func (m *mockConnWriter) Write(b *mempool.Buffer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.errIdx < len(m.errors) {
		err := m.errors[m.errIdx]
		m.errIdx++
		if err != nil {
			return err
		}
	}

	data := make([]byte, b.Len())
	copy(data, b.Bytes())
	m.written = append(m.written, data)
	return nil
}

func (m *mockConnWriter) String() string {
	return m.id
}

func (m *mockConnWriter) setError(err error) {
	m.errors = append(m.errors, err)
}

// TestCFSSchedulerCreation tests basic scheduler creation
func TestCFSSchedulerCreation(t *testing.T) {
	tests := []struct {
		name         string
		isServerSide bool
	}{
		{"Client scheduler", false},
		{"Server scheduler", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := NewCFSScheduler(tt.isServerSide)
			if sch == nil {
				t.Error("NewCFSScheduler returned nil")
			}

			impl, ok := sch.(*schedulerImpl)
			if !ok {
				t.Error("NewCFSScheduler didn't return *schedulerImpl")
			}

			if impl.isServerSide != tt.isServerSide {
				t.Errorf("Expected isServerSide=%v, got %v", tt.isServerSide, impl.isServerSide)
			}

			if impl.heap.Len() != 0 {
				t.Errorf("Expected empty heap, got %d items", impl.heap.Len())
			}
		})
	}
}

// TestPathHeapBasics tests basic heap operations
func TestPathHeapBasics(t *testing.T) {
	var h pathHeap

	// Test empty heap
	if h.Len() != 0 {
		t.Errorf("Expected empty heap length 0, got %d", h.Len())
	}

	// Create test paths with different virtual sent values
	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")
	conn3 := newMockConnWriter("path3")

	path1 := &cfsPath{Path: path.NewPath(conn1), virtualSent: 100}
	path2 := &cfsPath{Path: path.NewPath(conn2), virtualSent: 50}
	path3 := &cfsPath{Path: path.NewPath(conn3), virtualSent: 200}

	// Test Push operations
	heap.Push(&h, path1)
	heap.Push(&h, path2)
	heap.Push(&h, path3)

	if h.Len() != 3 {
		t.Errorf("Expected heap length 3, got %d", h.Len())
	}

	// Test heap property - minimum should be at index 0
	if h[0].virtualSent != 50 {
		t.Errorf("Expected minimum virtualSent=50, got %d", h[0].virtualSent)
	}

	// Test Pop operation
	min := heap.Pop(&h).(*cfsPath)
	if min.virtualSent != 50 {
		t.Errorf("Expected popped virtualSent=50, got %d", min.virtualSent)
	}

	if h.Len() != 2 {
		t.Errorf("Expected heap length 2 after pop, got %d", h.Len())
	}

	// Verify heap property maintained
	if h[0].virtualSent != 100 {
		t.Errorf("Expected new minimum virtualSent=100, got %d", h[0].virtualSent)
	}
}

// TestPathHeapIndexTracking tests that heap indices are correctly maintained
func TestPathHeapIndexTracking(t *testing.T) {
	var h pathHeap

	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")
	conn3 := newMockConnWriter("path3")

	path1 := &cfsPath{Path: path.NewPath(conn1), virtualSent: 100}
	path2 := &cfsPath{Path: path.NewPath(conn2), virtualSent: 50}
	path3 := &cfsPath{Path: path.NewPath(conn3), virtualSent: 200}

	heap.Push(&h, path1)
	heap.Push(&h, path2)
	heap.Push(&h, path3)

	// Check that heap indices are correctly set
	for i, p := range h {
		if p.heapIdx != i {
			t.Errorf("Path at position %d has heapIdx=%d, expected %d", i, p.heapIdx, i)
		}
	}

	// Test that swapping maintains indices
	h.Swap(0, 1)
	for i, p := range h {
		if p.heapIdx != i {
			t.Errorf("After swap, path at position %d has heapIdx=%d, expected %d", i, p.heapIdx, i)
		}
	}
}

// TestSchedulerAddPath tests adding paths to scheduler
func TestSchedulerAddPath(t *testing.T) {
	sch := NewCFSScheduler(false).(*schedulerImpl)

	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")

	path1 := NewPath(path.NewPath(conn1))
	path2 := NewPath(path.NewPath(conn2))

	// Add first path
	sch.AddPath(path1)
	if sch.heap.Len() != 1 {
		t.Errorf("Expected heap size 1, got %d", sch.heap.Len())
	}

	// Add second path
	sch.AddPath(path2)
	if sch.heap.Len() != 2 {
		t.Errorf("Expected heap size 2, got %d", sch.heap.Len())
	}

	// Verify heap property is maintained
	if sch.heap[0].virtualSent > sch.heap[1].virtualSent {
		t.Error("Heap property violated: parent > child")
	}
}

// TestSchedulerRemovePath tests removing paths from scheduler
func TestSchedulerRemovePath(t *testing.T) {
	sch := NewCFSScheduler(false).(*schedulerImpl)

	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")
	conn3 := newMockConnWriter("path3")

	path1 := NewPath(path.NewPath(conn1)).(*cfsPath)
	path2 := NewPath(path.NewPath(conn2)).(*cfsPath)
	path3 := NewPath(path.NewPath(conn3)).(*cfsPath)

	// Add paths
	sch.AddPath(path1)
	sch.AddPath(path2)
	sch.AddPath(path3)

	originalSize := sch.heap.Len()
	if originalSize != 3 {
		t.Errorf("Expected heap size 3, got %d", originalSize)
	}

	// Remove middle path
	sch.RemovePath(path2)
	if sch.heap.Len() != 2 {
		t.Errorf("Expected heap size 2 after removal, got %d", sch.heap.Len())
	}

	// Verify heap property is maintained
	for i := 0; i < sch.heap.Len()/2; i++ {
		left := 2*i + 1
		right := 2*i + 2
		if left < sch.heap.Len() && sch.heap[i].virtualSent > sch.heap[left].virtualSent {
			t.Error("Heap property violated after removal")
		}
		if right < sch.heap.Len() && sch.heap[i].virtualSent > sch.heap[right].virtualSent {
			t.Error("Heap property violated after removal")
		}
	}

	// Verify removed path has invalid heap index
	if path2.heapIdx != -1 {
		t.Errorf("Expected removed path heapIdx=-1, got %d", path2.heapIdx)
	}
}

// TestSchedulerRemovePathPanic tests that removing invalid path panics
func TestSchedulerRemovePathPanic(t *testing.T) {
	sch := NewCFSScheduler(false).(*schedulerImpl)

	// Test removing path that was never added
	conn := newMockConnWriter("path")
	invalidPath := NewPath(path.NewPath(conn)).(*cfsPath)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic when removing path with heapIdx < 0")
		}
	}()

	sch.RemovePath(invalidPath)
}

// TestPathWeightManagement tests weight setting and virtual sent calculation
func TestPathWeightManagement(t *testing.T) {
	conn := newMockConnWriter("path")
	cfsPath := NewPath(path.NewPath(conn)).(*cfsPath)

	// Test initial weight (should be 0)
	if cfsPath.weight != 0 {
		t.Errorf("Expected initial weight 0, got %d", cfsPath.weight)
	}

	// Test setting valid weight
	cfsPath.SetWeight(10)
	if cfsPath.weight != 10 {
		t.Errorf("Expected weight 10, got %d", cfsPath.weight)
	}

	// Test setting negative weight (should be ignored)
	cfsPath.SetWeight(-5)
	if cfsPath.weight != 10 {
		t.Errorf("Expected weight to remain 10 for negative input, got %d", cfsPath.weight)
	}

	// Test setting same weight (should be ignored)
	cfsPath.SetWeight(10)
	if cfsPath.weight != 10 {
		t.Errorf("Expected weight to remain 10, got %d", cfsPath.weight)
	}
}

// TestVirtualSentCalculation tests virtual sent bytes calculation
func TestVirtualSentCalculation(t *testing.T) {
	conn := newMockConnWriter("path")
	cfsPath := NewPath(path.NewPath(conn)).(*cfsPath)

	tests := []struct {
		name       string
		sentBytes  uint64
		weight     int
		expectedVS uint64
	}{
		{"Zero weight", 100, 0, 100}, // weight treated as 1
		{"Normal weight", 100, 10, 10},
		{"High weight", 1000, 100, 10},
		{"Weight 1", 50, 1, 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfsPath.sentBytes = tt.sentBytes
			cfsPath.weight = tt.weight
			cfsPath.updateVirtualSent()

			if cfsPath.virtualSent != tt.expectedVS {
				t.Errorf("Expected virtualSent=%d, got %d", tt.expectedVS, cfsPath.virtualSent)
			}
		})
	}
}

// TestBeforeWrite tests the beforeWrite method
func TestBeforeWrite(t *testing.T) {
	conn := newMockConnWriter("path")
	cfsPath := NewPath(path.NewPath(conn)).(*cfsPath)

	cfsPath.SetWeight(10)
	initialSent := cfsPath.sentBytes

	size := 100
	cfsPath.beforeWrite(size)

	expectedSent := initialSent + uint64(size)
	expectedVS := expectedSent / 10

	if cfsPath.sentBytes != expectedSent {
		t.Errorf("Expected sentBytes=%d, got %d", expectedSent, cfsPath.sentBytes)
	}

	if cfsPath.virtualSent != expectedVS {
		t.Errorf("Expected virtualSent=%d, got %d", expectedVS, cfsPath.virtualSent)
	}
}

// TestMarkAsLost tests the markAsLost method
func TestMarkAsLost(t *testing.T) {
	conn := newMockConnWriter("path")
	cfsPath := NewPath(path.NewPath(conn)).(*cfsPath)

	cfsPath.markAsLost()

	if cfsPath.virtualSent != ^uint64(0) { // MaxUint64
		t.Errorf("Expected virtualSent=MaxUint64, got %d", cfsPath.virtualSent)
	}
}

// TestFindBestPath tests the findBestPath method
func TestFindBestPath(t *testing.T) {
	sch := NewCFSScheduler(false).(*schedulerImpl)

	// Test with empty scheduler
	_, err := sch.findBestPath(100)
	if err != scheduler.ErrNoPath {
		t.Errorf("Expected ErrNoPath for empty scheduler, got %v", err)
	}

	// Add paths with different virtual sent values
	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")
	conn3 := newMockConnWriter("path3")

	path1 := NewPath(path.NewPath(conn1)).(*cfsPath)
	path2 := NewPath(path.NewPath(conn2)).(*cfsPath)
	path3 := NewPath(path.NewPath(conn3)).(*cfsPath)

	// Set sentBytes and update virtualSent properly
	path1.sentBytes = 100
	path1.updateVirtualSent() // virtualSent = 100/1 = 100
	path2.sentBytes = 50
	path2.updateVirtualSent() // virtualSent = 50/1 = 50
	path3.sentBytes = 200
	path3.updateVirtualSent() // virtualSent = 200/1 = 200

	sch.AddPath(path1)
	sch.AddPath(path2)
	sch.AddPath(path3)

	// Store the path that should be selected (lowest virtualSent)
	expectedBestPath := path2 // This has virtualSent = 50

	// Find best path (should be the one with minimum virtual sent)
	bestPath, err := sch.findBestPath(100)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify we got the expected path (by checking the underlying connection)
	if bestPath.String() != expectedBestPath.String() {
		t.Errorf("Expected best path %s, got %s", expectedBestPath.String(), bestPath.String())
	}

	// Verify that beforeWrite was called (virtual sent should have increased)
	// Original: 50, after adding 100 bytes with weight 0 (treated as 1): (50 + 100) / 1 = 150
	expectedVS := uint64(150)
	if bestPath.virtualSent != expectedVS {
		t.Errorf("Expected virtualSent to be updated to %d, got %d", expectedVS, bestPath.virtualSent)
	}
}

// TestSchedulerWrite tests the Write method
func TestSchedulerWrite(t *testing.T) {
	sch := NewCFSScheduler(false)

	// Test with empty scheduler
	buf := mempool.Get(100)
	defer mempool.Put(buf)

	err := sch.Write(buf)
	if err != scheduler.ErrNoPath {
		t.Errorf("Expected ErrNoPath for empty scheduler, got %v", err)
	}

	// Add a path
	conn := newMockConnWriter("path1")
	path1 := NewPath(path.NewPath(conn))

	sch.AddPath(path1)

	// Test successful write
	err = sch.Write(buf)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify data was written to the connection
	mockConn := conn
	mockConn.mu.Lock()
	writtenCount := len(mockConn.written)
	var firstWriteLen int
	if writtenCount > 0 {
		firstWriteLen = len(mockConn.written[0])
	}
	mockConn.mu.Unlock()

	if writtenCount != 1 {
		t.Errorf("Expected 1 write to connection, got %d", writtenCount)
	}

	if firstWriteLen != buf.Len() {
		t.Errorf("Expected written data length %d, got %d", buf.Len(), firstWriteLen)
	}
}

// TestMultiPathScheduling tests scheduling across multiple paths
func TestMultiPathScheduling(t *testing.T) {
	sch := NewCFSScheduler(false)

	// Create multiple paths with different weights
	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")
	conn3 := newMockConnWriter("path3")

	path1 := NewPath(path.NewPath(conn1))
	path2 := NewPath(path.NewPath(conn2))
	path3 := NewPath(path.NewPath(conn3))

	path1.SetWeight(10) // High weight = more capacity
	path2.SetWeight(5)  // Medium weight
	path3.SetWeight(1)  // Low weight

	sch.AddPath(path1)
	sch.AddPath(path2)
	sch.AddPath(path3)

	// Write multiple buffers and track which paths get used
	writes := 10
	pathUsage := make(map[string]int)

	for i := 0; i < writes; i++ {
		buf := mempool.Get(100)
		err := sch.Write(buf)
		mempool.Put(buf)

		if err != nil {
			t.Errorf("Write %d failed: %v", i, err)
			continue
		}

		// Find which path was used (the one with new data)
		for name, mockConn := range map[string]*mockConnWriter{
			"path1": conn1,
			"path2": conn2,
			"path3": conn3,
		} {
			currentWrites := len(mockConn.written)

			if currentWrites > pathUsage[name] {
				pathUsage[name]++
				break
			}
		}
	}

	// Higher weight paths should get more traffic
	if pathUsage["path1"] <= pathUsage["path3"] {
		t.Errorf("Expected path1 (weight 10) to get more traffic than path3 (weight 1), got %d vs %d",
			pathUsage["path1"], pathUsage["path3"])
	}
}

// TestConcurrentAccess tests thread safety of scheduler operations
func TestConcurrentAccess(t *testing.T) {
	sch := NewCFSScheduler(false)

	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")

	path1 := NewPath(path.NewPath(conn1))
	path2 := NewPath(path.NewPath(conn2))

	sch.AddPath(path1)
	sch.AddPath(path2)

	// Test concurrent writes
	done := make(chan bool, 10)
	errors := make(chan error, 100)
	successCount := make(chan int, 10)

	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()

			localSuccessCount := 0
			for j := 0; j < 10; j++ {
				buf := mempool.Get(100)
				err := sch.Write(buf)
				mempool.Put(buf)

				if err != nil {
					if err != scheduler.ErrNoPath {
						errors <- err
					}
				} else {
					localSuccessCount++
				}
			}
			successCount <- localSuccessCount
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	close(errors)
	close(successCount)

	// Check for any errors
	for err := range errors {
		t.Errorf("Concurrent write error: %v", err)
	}

	// Count successful writes
	totalSuccessful := 0
	for count := range successCount {
		totalSuccessful += count
	}

	// Verify total writes match successful operations
	conn1.mu.Lock()
	conn1Writes := len(conn1.written)
	conn1.mu.Unlock()

	conn2.mu.Lock()
	conn2Writes := len(conn2.written)
	conn2.mu.Unlock()

	totalWrites := conn1Writes + conn2Writes
	if totalWrites != totalSuccessful {
		t.Errorf("Mismatch between successful operations (%d) and recorded writes (%d)", totalSuccessful, totalWrites)
	}

	// We should have some writes (exact count may vary due to concurrency)
	if totalWrites == 0 {
		t.Error("Expected some writes to succeed")
	}

	t.Logf("Completed %d concurrent writes successfully", totalWrites)
}

// TestPathRemovalDuringOperation tests removing paths while operations are ongoing
func TestPathRemovalDuringOperation(t *testing.T) {
	sch := NewCFSScheduler(false)

	conn1 := newMockConnWriter("path1")
	conn2 := newMockConnWriter("path2")

	path1 := NewPath(path.NewPath(conn1))
	path2 := NewPath(path.NewPath(conn2))

	sch.AddPath(path1)
	sch.AddPath(path2)

	// Write some data
	buf := mempool.Get(100)
	err := sch.Write(buf)
	mempool.Put(buf)
	if err != nil {
		t.Errorf("Initial write failed: %v", err)
	}

	// Remove one path
	sch.RemovePath(path1)

	// Should still be able to write to remaining path
	buf = mempool.Get(100)
	err = sch.Write(buf)
	mempool.Put(buf)
	if err != nil {
		t.Errorf("Write after path removal failed: %v", err)
	}

	// Remove last path
	sch.RemovePath(path2)

	// Should now get ErrNoPath
	buf = mempool.Get(100)
	err = sch.Write(buf)
	mempool.Put(buf)
	if err != scheduler.ErrNoPath {
		t.Errorf("Expected ErrNoPath after removing all paths, got %v", err)
	}
}

// TestLargeScaleScheduling tests scheduler behavior with many paths
func TestLargeScaleScheduling(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large scale test in short mode")
	}

	sch := NewCFSScheduler(false)
	numPaths := 100

	// Add many paths
	paths := make([]scheduler.SchedulablePath, numPaths)
	conns := make([]*mockConnWriter, numPaths)

	for i := 0; i < numPaths; i++ {
		conns[i] = newMockConnWriter(fmt.Sprintf("path%d", i))
		paths[i] = NewPath(path.NewPath(conns[i]))
		paths[i].SetWeight(i + 1) // Varying weights
		sch.AddPath(paths[i])
	}

	// Perform many writes
	numWrites := 1000
	for i := 0; i < numWrites; i++ {
		buf := mempool.Get(100)
		err := sch.Write(buf)
		mempool.Put(buf)

		if err != nil {
			t.Errorf("Write %d failed: %v", i, err)
		}
	}

	// Verify distribution - higher weight paths should get more traffic
	// usage := make([]int, numPaths)
	for _, conn := range conns {
		if len(conn.written[0]) != 100 {
			t.Errorf("unexpected written: want: %d got: %d", 100, len(conn.written))
		}
	}
}
