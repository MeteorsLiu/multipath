package transport

import (
	"net"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
)

type udpSocketBufferState struct {
	actualRead    int
	actualReadOK  bool
	actualWrite   int
	actualWriteOK bool

	readErr       error
	writeErr      error
	forceReadErr  error
	forceWriteErr error
}

func tunePacketConn(endpointID string, conn net.PacketConn) {
	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		return
	}

	state := setUDPSocketBuffer(udpConn, udpSocketBufferSize)
	debuglog.Printf("transport/udp", "buffer endpoint=%s requested=%d actual_read=%d actual_read_ok=%t actual_write=%d actual_write_ok=%t read_err=%v write_err=%v force_read_err=%v force_write_err=%v",
		endpointID, udpSocketBufferSize,
		state.actualRead, state.actualReadOK,
		state.actualWrite, state.actualWriteOK,
		state.readErr, state.writeErr,
		state.forceReadErr, state.forceWriteErr)
}
