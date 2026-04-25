package transport

import (
	"fmt"
	"net"
)

func debugKind(kind Kind) string {
	switch kind {
	case KindUDP:
		return "udp"
	case KindTCP:
		return "tcp"
	default:
		return fmt.Sprintf("unknown(%d)", kind)
	}
}

func debugLeg(leg LegRef) string {
	switch leg.Kind {
	case KindUDP:
		remote := "<nil>"
		if leg.RemoteAddr != nil {
			remote = leg.RemoteAddr.String()
		}
		return fmt.Sprintf("udp endpoint=%s remote=%s", leg.EndpointID, remote)
	case KindTCP:
		return fmt.Sprintf("tcp conn=%s", leg.ConnID)
	default:
		return fmt.Sprintf("kind=%d", leg.Kind)
	}
}

func debugRemoteAddr(conn net.Conn) (addr net.Addr) {
	if conn == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			addr = nil
		}
	}()
	return conn.RemoteAddr()
}

func debugLocalAddr(conn net.Conn) (addr net.Addr) {
	if conn == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			addr = nil
		}
	}()
	return conn.LocalAddr()
}
