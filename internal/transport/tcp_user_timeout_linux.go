//go:build linux

package transport

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func configureTCPConn(conn net.Conn, timeout time.Duration) error {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok || timeout <= 0 {
		return nil
	}

	timeoutMS := int(timeout / time.Millisecond)
	if timeoutMS <= 0 {
		return nil
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return err
	}

	var setErr error
	if err := rawConn.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, timeoutMS)
	}); err != nil {
		return err
	}
	return setErr
}
