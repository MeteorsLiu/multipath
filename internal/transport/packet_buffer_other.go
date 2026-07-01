//go:build !linux

package transport

import (
	"net"
	"syscall"
)

func setUDPSocketBuffer(conn *net.UDPConn, size int) udpSocketBufferState {
	state := udpSocketBufferState{
		readErr:  conn.SetReadBuffer(size),
		writeErr: conn.SetWriteBuffer(size),
	}
	state.actualRead, state.actualReadOK = udpSocketBuffer(conn, syscall.SO_RCVBUF)
	state.actualWrite, state.actualWriteOK = udpSocketBuffer(conn, syscall.SO_SNDBUF)
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
		value, sysErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, name)
	}); err != nil || sysErr != nil {
		return 0, false
	}
	return value, true
}
