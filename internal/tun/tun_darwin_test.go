//go:build darwin

package tun

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDarwinUTUNStripsAndAddsAddressFamilyPrefix(t *testing.T) {
	var input bytes.Buffer
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], darwinAFInet)
	input.Write(prefix[:])
	input.Write([]byte{0x45, 0, 0, 20})

	rw := &memoryTun{read: &input}
	utun := newDarwinUTUN(rw)
	packet := make([]byte, 1500)
	n, err := utun.Read(packet)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 4 || packet[0] != 0x45 {
		t.Fatalf("packet = n %d bytes %#v, want IPv4 bytes without prefix", n, packet[:n])
	}

	n, err = utun.Write(packet[:n])
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 4 {
		t.Fatalf("n = %d, want 4", n)
	}
	got := rw.write.Bytes()
	if len(got) != 8 {
		t.Fatalf("written len = %d, want 8", len(got))
	}
	if family := binary.BigEndian.Uint32(got[:4]); family != darwinAFInet {
		t.Fatalf("family = %d, want %d", family, darwinAFInet)
	}
}
