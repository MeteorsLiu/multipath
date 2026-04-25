//go:build linux

package tun

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const linuxCloneDevicePath = "/dev/net/tun"

func Open(name string, mtu int) (*Device, error) {
	fd, err := unix.Open(linuxCloneDevicePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), linuxCloneDevicePath)
	device := newDevice(file, mtu, ifr.Name())
	if mtu > 0 {
		if err := Configure(device.Name(), "", "", mtu); err != nil {
			_ = device.Close()
			return nil, err
		}
	}
	return device, nil
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
