//go:build linux

package tun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"golang.org/x/sys/unix"
)

const linuxCloneDevicePath = "/dev/net/tun"

func Open(name string, mtu int) (*Device, error) {
	device, err := openLinuxTUN(name, mtu, true)
	if err == nil {
		return device, nil
	}
	if !errors.Is(err, errLinuxTUNOffloadUnsupported) {
		return nil, err
	}
	return openLinuxTUN(name, mtu, false)
}

var errLinuxTUNOffloadUnsupported = errors.New("tun: linux vnet offload unsupported")

const (
	tunTCPOffloads = unix.TUN_F_CSUM | unix.TUN_F_TSO4 | unix.TUN_F_TSO6
	tunUDPOffloads = unix.TUN_F_USO4 | unix.TUN_F_USO6
)

func openLinuxTUN(name string, mtu int, vnet bool) (*Device, error) {
	fd, err := unix.Open(linuxCloneDevicePath, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	flags := uint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if vnet {
		flags |= unix.IFF_VNET_HDR
	}
	ifr.SetUint16(flags)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		if vnet && errors.Is(err, unix.EINVAL) {
			return nil, errLinuxTUNOffloadUnsupported
		}
		return nil, err
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), linuxCloneDevicePath)
	native := newLinuxNativeTun(file, vnet, mtu)
	if vnet {
		if err := native.enableOffloads(); err != nil {
			_ = native.Close()
			return nil, err
		}
	}
	device := newDevice(native, mtu, ifr.Name())
	if mtu > 0 {
		if err := Configure(device.Name(), "", "", mtu); err != nil {
			_ = device.Close()
			return nil, err
		}
	}
	return device, nil
}

type linuxNativeTun struct {
	file *os.File
	mtu  int

	vnetHdr bool
	udpGSO  bool

	readMu   sync.Mutex
	readBuff [virtioNetHdrLen + 65535]byte

	writeMu     sync.Mutex
	tcpGROTable *tcpGROTable
	udpGROTable *udpGROTable
	toWrite     []int
	writeBufs   [][]byte
	writeLens   []int
}

func newLinuxNativeTun(file *os.File, vnetHdr bool, mtu int) *linuxNativeTun {
	if mtu <= 0 {
		mtu = defaultMTU
	}
	return &linuxNativeTun{
		file:        file,
		mtu:         mtu,
		vnetHdr:     vnetHdr,
		tcpGROTable: newTCPGROTable(),
		udpGROTable: newUDPGROTable(),
		toWrite:     make([]int, 0, tunIdealBatchSize),
		writeBufs:   make([][]byte, 0, tunIdealBatchSize),
		writeLens:   make([]int, 0, tunIdealBatchSize),
	}
}

func (t *linuxNativeTun) enableOffloads() error {
	fd := int(t.file.Fd())
	if err := unix.IoctlSetInt(fd, unix.TUNSETOFFLOAD, tunTCPOffloads); err != nil {
		return fmt.Errorf("%w: %v", errLinuxTUNOffloadUnsupported, err)
	}
	t.udpGSO = unix.IoctlSetInt(fd, unix.TUNSETOFFLOAD, tunTCPOffloads|tunUDPOffloads) == nil
	return nil
}

func (t *linuxNativeTun) BatchSize() int {
	if !t.vnetHdr {
		return 1
	}
	return tunIdealBatchSize
}

func (t *linuxNativeTun) Read(p []byte) (int, error) {
	if !t.vnetHdr {
		return t.file.Read(p)
	}

	packet := packetbuf.Acquire(len(p))
	defer packet.Release()
	packets := []*packetbuf.Packet{packet}
	n, err := t.ReadPackets(packets)
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("tun: vnet read returned %d packets for single read", n)
	}
	if len(p) < len(packet.Payload) {
		return 0, io.ErrShortBuffer
	}
	return copy(p, packet.Payload), nil
}

func (t *linuxNativeTun) ReadPackets(packets []*packetbuf.Packet) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	if !t.vnetHdr {
		if packets[0] == nil {
			packets[0] = packetbuf.Acquire(t.mtu)
		}
		n, err := t.file.Read(packets[0].Payload)
		if err != nil {
			return 0, err
		}
		packets[0].SetLen(n)
		return 1, nil
	}

	t.readMu.Lock()
	defer t.readMu.Unlock()

	n, err := t.file.Read(t.readBuff[:])
	if err != nil {
		return 0, err
	}
	if packets[0] == nil {
		packets[0] = packetbuf.Acquire(t.mtu)
	}
	if virtioReadHasGSO(t.readBuff[:n]) {
		for i := 1; i < len(packets); i++ {
			if packets[i] == nil {
				packets[i] = packetbuf.Acquire(t.mtu)
			}
		}
	}
	bufs := make([][]byte, len(packets))
	sizes := make([]int, len(packets))
	for i := range packets {
		if packets[i] != nil {
			bufs[i] = packets[i].Payload
		}
	}
	count, err := handleVirtioRead(t.readBuff[:n], bufs, sizes, 0)
	if count < 0 {
		count = 0
	}
	for i := 0; i < count; i++ {
		packets[i].SetLen(sizes[i])
	}
	return count, err
}

func virtioReadHasGSO(packet []byte) bool {
	var hdr virtioNetHdr
	if hdr.decode(packet) != nil {
		return false
	}
	return hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_NONE
}

func (t *linuxNativeTun) Write(p []byte) (int, error) {
	if !t.vnetHdr {
		return t.file.Write(p)
	}

	packet := packetbuf.Acquire(len(p))
	copy(packet.Payload, p)
	packet.SetLen(len(p))
	defer packet.Release()
	n, err := t.WritePackets([]*packetbuf.Packet{packet})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return len(p), nil
}

func (t *linuxNativeTun) WritePackets(packets []*packetbuf.Packet) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	if !t.vnetHdr {
		written := 0
		for _, packet := range packets {
			if packet == nil {
				continue
			}
			if _, err := t.file.Write(packet.Payload); err != nil {
				return written, err
			}
			written++
		}
		return written, nil
	}

	t.writeMu.Lock()
	defer func() {
		t.tcpGROTable.reset()
		t.udpGROTable.reset()
		t.writeMu.Unlock()
	}()

	bufs := t.prepareWriteBuffers(len(packets))
	count := 0
	for _, packet := range packets {
		if packet == nil {
			continue
		}
		if len(packet.Payload) > 65535 {
			return 0, fmt.Errorf("tun: packet length %d exceeds maximum IP packet size", len(packet.Payload))
		}
		n := virtioNetHdrLen + len(packet.Payload)
		buf := bufs[count][:n]
		clear(buf[:virtioNetHdrLen])
		copy(buf[virtioNetHdrLen:], packet.Payload)
		bufs[count] = buf
		t.writeLens[count] = len(packet.Payload)
		count++
	}
	if count == 0 {
		return 0, nil
	}
	bufs = bufs[:count]
	t.writeLens = t.writeLens[:count]

	t.toWrite = t.toWrite[:0]
	if err := handleGRO(bufs, virtioNetHdrLen, t.tcpGROTable, t.udpGROTable, t.udpGSO, &t.toWrite); err != nil {
		return 0, err
	}

	written := 0
	for _, idx := range t.toWrite {
		if t.writeLens[idx] == 0 {
			continue
		}
		if _, err := t.file.Write(bufs[idx]); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func (t *linuxNativeTun) prepareWriteBuffers(n int) [][]byte {
	if cap(t.writeBufs) < n {
		t.writeBufs = make([][]byte, n)
	} else {
		t.writeBufs = t.writeBufs[:n]
	}
	if cap(t.writeLens) < n {
		t.writeLens = make([]int, n)
	} else {
		t.writeLens = t.writeLens[:n]
		clear(t.writeLens)
	}
	for i := range t.writeBufs {
		if cap(t.writeBufs[i]) < virtioNetHdrLen+65535 {
			t.writeBufs[i] = make([]byte, virtioNetHdrLen+65535)
		}
	}
	return t.writeBufs
}

func (t *linuxNativeTun) Close() error {
	return t.file.Close()
}

func Configure(name string, localAddr string, remoteAddr string, mtu int) error {
	if name == "" {
		return nil
	}
	if localAddr != "" {
		args := []string{"addr", "add", localAddr}
		if remoteAddr != "" {
			args = append(args, "peer", remoteAddr)
		}
		args = append(args, "dev", name)
		if err := runCommand("ip", args...); err != nil {
			return fmt.Errorf("configure address for %s: %w", name, err)
		}
	}
	if mtu > 0 {
		if err := runCommand("ip", "link", "set", "dev", name, "mtu", strconv.Itoa(mtu)); err != nil {
			return fmt.Errorf("set mtu for %s: %w", name, err)
		}
	}
	return runCommand("ip", "link", "set", "dev", name, "up")
}

func configureRoutes(name string, allowedIPs []string) error {
	for _, cidr := range allowedIPs {
		if cidr == "" {
			continue
		}
		if err := runCommand("ip", "route", "replace", cidr, "dev", name); err != nil {
			return fmt.Errorf("route %s via %s: %w", cidr, name, err)
		}
	}
	return nil
}
