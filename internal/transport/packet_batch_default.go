//go:build !linux

package transport

import "net"

func newPacketBatcher(net.PacketConn) packetBatcher {
	return nil
}
