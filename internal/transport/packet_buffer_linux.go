//go:build linux

package transport

import (
	"net"

	"golang.org/x/sys/unix"
)

func setUDPSocketBuffer(conn *net.UDPConn, size int) udpSocketBufferState {
	state := udpSocketBufferState{
		readErr:  conn.SetReadBuffer(size),
		writeErr: conn.SetWriteBuffer(size),
	}

	rawConn, err := conn.SyscallConn()
	if err == nil {
		_ = rawConn.Control(func(fd uintptr) {
			state.forceReadErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, size)
			state.forceWriteErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, size)
		})
	}
	state.actualRead, state.actualReadOK = udpSocketBuffer(conn, unix.SO_RCVBUF)
	state.actualWrite, state.actualWriteOK = udpSocketBuffer(conn, unix.SO_SNDBUF)
	return state
}

func udpSocketBuffer(conn *net.UDPConn, name int) (int, bool) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}

	var value int
	var sysErr error
	if err := rawConn.Control(func(fd uintptr) {
		value, sysErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, name)
	}); err != nil || sysErr != nil {
		return 0, false
	}
	return value, true
}
