//go:build darwin

package tun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	darwinSysProtoControl = 2
	darwinUTUNOptIFName   = 2
	darwinUTUNControlName = "com.apple.net.utun_control"
	darwinAFInet          = 2
)

var ErrUnsupportedPacketFamily = errors.New("tun: unsupported packet family")

func Open(name string, mtu int) (*Device, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, darwinSysProtoControl)
	if err != nil {
		return nil, err
	}

	ctlInfo := &unix.CtlInfo{}
	copy(ctlInfo.Name[:], darwinUTUNControlName)
	if err := unix.IoctlCtlInfo(fd, ctlInfo); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	if err := unix.Connect(fd, &unix.SockaddrCtl{
		ID:   ctlInfo.Id,
		Unit: darwinUnit(name),
	}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	ifName, err := unix.GetsockoptString(fd, darwinSysProtoControl, darwinUTUNOptIFName)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), ifName)
	return newDevice(newDarwinUTUN(file), mtu, ifName), nil
}

func darwinUnit(name string) uint32 {
	if !strings.HasPrefix(name, "utun") {
		return 0
	}
	index, err := strconv.Atoi(strings.TrimPrefix(name, "utun"))
	if err != nil || index < 0 {
		return 0
	}
	return uint32(index + 1)
}

func Configure(name string, localAddr string, remoteAddr string, mtu int) error {
	if name == "" || localAddr == "" || remoteAddr == "" {
		return nil
	}
	args := []string{name, "inet", localAddr, remoteAddr}
	if mtu > 0 {
		args = append(args, "mtu", strconv.Itoa(mtu))
	}
	return runCommand("ifconfig", args...)
}

func configureRoutes(name string, allowedIPs []string) error {
	for _, cidr := range allowedIPs {
		if cidr == "" {
			continue
		}
		if err := runCommand("route", "-n", "add", "-net", cidr, "-interface", name); err != nil {
			return fmt.Errorf("route %s via %s: %w", cidr, name, err)
		}
	}
	return nil
}

type darwinUTUN struct {
	rw io.ReadWriteCloser
}

func newDarwinUTUN(rw io.ReadWriteCloser) *darwinUTUN {
	return &darwinUTUN{rw: rw}
}

func (d *darwinUTUN) Read(p []byte) (int, error) {
	buf := make([]byte, len(p)+4)
	n, err := d.rw.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	if family := binary.BigEndian.Uint32(buf[:4]); family != darwinAFInet {
		return 0, ErrUnsupportedPacketFamily
	}
	return copy(p, buf[4:n]), nil
}

func (d *darwinUTUN) Write(p []byte) (int, error) {
	if len(p) == 0 || p[0]>>4 != 4 {
		return 0, ErrUnsupportedPacketFamily
	}

	buf := make([]byte, 4+len(p))
	binary.BigEndian.PutUint32(buf[:4], darwinAFInet)
	copy(buf[4:], p)
	n, err := d.rw.Write(buf)
	if n <= 4 {
		return 0, err
	}
	return n - 4, err
}

func (d *darwinUTUN) Close() error {
	return d.rw.Close()
}
