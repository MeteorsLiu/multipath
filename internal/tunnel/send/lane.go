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

	helloCaps       uint16
	helloFECProfile uint8

	fallbackDialing bool
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

func (l *laneRuntime) Weight() uint32 {
	if l == nil {
		return 0
	}
	return l.weight
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
