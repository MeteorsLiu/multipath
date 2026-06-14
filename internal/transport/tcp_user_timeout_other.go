//go:build !linux

package transport

import (
	"net"
	"time"
)

func configureTCPConn(conn net.Conn, timeout time.Duration) error {
	return nil
}
