package send

import (
	"errors"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

var errLaneUnavailable = errors.New("tunnel: lane has no usable transport leg")

type laneRuntime struct {
	id     uint8
	weight uint32

	udpReady bool
	udpLeg   transport.LegRef

	tcpReady  bool
	tcpLeg    transport.LegRef
	tcpRemote string

	helloRetry helloRetry

	fallbackDialing bool

	queued bool
}

func newLaneRuntime(id uint8, weight uint32) *laneRuntime {
	return &laneRuntime{
		id:     id,
		weight: weight,
	}
}

func (l *laneRuntime) ready() bool {
	if l == nil {
		return false
	}
	_, ok := l.selectLeg()
	return ok
}

func (l *laneRuntime) selectLeg() (transport.LegRef, bool) {
	if l.udpReady && l.udpLeg.EndpointID != "" && l.udpLeg.RemoteAddr != nil {
		return l.udpLeg, true
	}
	if l.tcpReady && l.tcpLeg.ConnID != "" {
		return l.tcpLeg, true
	}
	return transport.LegRef{}, false
}

func legCharge(leg transport.LegRef, payloadLen int) uint32 {
	if leg.Kind == transport.KindTCP {
		return uint32(payloadLen + 2)
	}
	return uint32(payloadLen)
}

func (l *laneRuntime) observeLeg(leg transport.LegRef) {
	l.bindLeg(leg)
}

func (l *laneRuntime) rememberLeg(leg transport.LegRef) {
	switch leg.Kind {
	case transport.KindUDP:
		l.udpLeg = leg
	case transport.KindTCP:
		l.tcpLeg = leg
	}
}

func (l *laneRuntime) bindLeg(leg transport.LegRef) {
	switch leg.Kind {
	case transport.KindUDP:
		l.udpLeg = leg
		l.udpReady = leg.EndpointID != "" && leg.RemoteAddr != nil
	case transport.KindTCP:
		l.tcpLeg = leg
		l.tcpReady = leg.ConnID != ""
	}
}

func (l *laneRuntime) canProbe(leg transport.LegRef) bool {
	switch leg.Kind {
	case transport.KindUDP:
		if leg.EndpointID == "" || leg.RemoteAddr == nil {
			return false
		}
	case transport.KindTCP:
		if !l.tcpReady || leg.ConnID == "" {
			return false
		}
	default:
		return false
	}
	return !l.helloRetry.pending() || !sameLeg(l.helloRetry.leg, leg)
}

func sameLeg(a transport.LegRef, b transport.LegRef) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case transport.KindUDP:
		if a.EndpointID != b.EndpointID {
			return false
		}
		if a.RemoteAddr == nil || b.RemoteAddr == nil {
			return a.RemoteAddr == b.RemoteAddr
		}
		return a.RemoteAddr.String() == b.RemoteAddr.String()
	case transport.KindTCP:
		return a.ConnID == b.ConnID
	default:
		return false
	}
}
