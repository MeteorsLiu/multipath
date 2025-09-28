package cfs

import (
	"bytes"
	"container/heap"
	"crypto/rand"
	"io"
	"testing"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/MeteorsLiu/multipath/internal/path"
)

func TestPath(t *testing.T) {
	sche := NewCFSScheduler()
	manager := path.NewManager()

	schePath1 := NewPath("1")
	schePath2 := NewPath("2")
	schePath3 := NewPath("3")
	sche.AddPath(schePath1)
	sche.AddPath(schePath2)
	sche.AddPath(schePath3)

	m1 := newMockConnWriter("1-1")
	m2 := newMockConnWriter("2-1")
	m3 := newMockConnWriter("3-1")
	m4 := newMockConnWriter("2-2")

	c1, _ := manager.Add("1-1", func() (w conn.ConnWriter, onRemove func()) {
		return m1, nil
	})
	c2, _ := manager.Add("2-1", func() (w conn.ConnWriter, onRemove func()) {
		return m2, nil
	})
	c3, _ := manager.Add("3-1", func() (w conn.ConnWriter, onRemove func()) {
		return m3, nil
	})
	c4, _ := manager.Add("2-2", func() (w conn.ConnWriter, onRemove func()) {
		return m4, nil
	})

	t.Run("No path", func(t *testing.T) {
		if err := sche.Write(mempool.Get(1)); err == nil {
			t.Error("unexpected err: should not be nil")
		}
	})

	t.Run("One path", func(t *testing.T) {
		schePath2.AddConnPath(c2)
		b := mempool.Get(100)
		io.ReadFull(rand.Reader, b.Bytes())
		if err := sche.Write(b); err != nil {
			t.Error("unexpected err: should be nil")
		}
		if !bytes.Equal(m2.written[0], b.Bytes()) {
			t.Error("unexpected buf")
		}
	})

	t.Run("Two paths", func(t *testing.T) {
		m1.reset()
		m2.reset()

		b1 := mempool.Get(100)
		b2 := mempool.Get(100)
		schePath1.AddConnPath(c1)

		io.ReadFull(rand.Reader, b1.Bytes())
		io.ReadFull(rand.Reader, b2.Bytes())

		if err := sche.Write(b1); err != nil {
			t.Error("unexpected err: should be nil")
		}
		if !bytes.Equal(m1.written[0], b1.Bytes()) {
			t.Error("unexpected buf")
		}
		if err := sche.Write(b2); err != nil {
			t.Error("unexpected err: should be nil")
		}
		if !bytes.Equal(m2.written[0], b2.Bytes()) {
			t.Error("unexpected buf")
		}
	})

	t.Run("Three paths", func(t *testing.T) {
		m1.reset()
		m2.reset()
		m3.reset()

		b1 := mempool.Get(100)

		b2 := mempool.Get(100)

		b3 := mempool.Get(100)

		schePath3.AddConnPath(c3)

		io.ReadFull(rand.Reader, b1.Bytes())

		io.ReadFull(rand.Reader, b2.Bytes())

		io.ReadFull(rand.Reader, b3.Bytes())

		t.Log(sche)

		if err := sche.Write(b3); err != nil {
			t.Error("unexpected err: should be nil")
		}
		if !bytes.Equal(m3.written[0], b3.Bytes()) {
			t.Error("unexpected buf")
		}
		t.Log(sche)

		if err := sche.Write(b1); err != nil {
			t.Error("unexpected err: should be nil")
		}

		if !bytes.Equal(m3.written[1], b1.Bytes()) {
			t.Error("unexpected buf")
		}

		if err := sche.Write(b2); err != nil {
			t.Error("unexpected err: should be nil")
		}

		if !bytes.Equal(m1.written[0], b2.Bytes()) {
			t.Error("unexpected buf")
		}

		t.Log(sche)

	})

	t.Run("Path Multiple Writer", func(t *testing.T) {
		m1.reset()
		m2.reset()
		m3.reset()

		schePath2.(*cfsPath).setVirtualSent(0)
		heap.Fix(&sche.(*schedulerImpl).heap, schePath2.(*cfsPath).heapIdx)

		schePath2.AddConnPath(c4)

		b1 := mempool.Get(100)

		b2 := mempool.Get(150)

		b3 := mempool.Get(100)

		io.ReadFull(rand.Reader, b1.Bytes())

		io.ReadFull(rand.Reader, b2.Bytes())

		io.ReadFull(rand.Reader, b3.Bytes())

		if err := sche.Write(b2); err != nil {
			t.Error("unexpected err: should be nil")
		}

		if err := sche.Write(b1); err != nil {
			t.Error("unexpected err: should be nil")
		}

		if !bytes.Equal(m2.written[0], b2.Bytes()) {
			t.Error("unexpected buf")
		}
		if !bytes.Equal(m4.written[0], b1.Bytes()) {
			t.Error("unexpected buf")
		}

		if err := sche.Write(b3); err != nil {
			t.Error("unexpected err: should be nil")
		}
		if !bytes.Equal(m1.written[0], b3.Bytes()) {
			t.Error("unexpected buf")
		}
	})
}
