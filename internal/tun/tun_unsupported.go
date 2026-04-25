//go:build !darwin && !linux

package tun

import "runtime"

func Open(name string, mtu int) (*Device, error) {
	return nil, ErrUnsupportedOS
}

func Configure(name string, localAddr string, remoteAddr string, mtu int) error {
	return ErrUnsupportedOS
}

func configureRoutes(name string, allowedIPs []string) error {
	return ErrUnsupportedOS
}

var ErrUnsupportedOS = unsupportedOSError(runtime.GOOS)

type unsupportedOSError string

func (e unsupportedOSError) Error() string {
	return "tun: unsupported OS " + string(e)
}
