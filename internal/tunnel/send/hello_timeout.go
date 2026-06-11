package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	tcpHelloInitialTimeoutMS = 1000
	tcpHelloMinTimeoutMS     = 1000
	tcpHelloMaxTimeoutMS     = 5000
	helloRTOGranularityMS    = 100
)

func (l *Send) helloTimeoutForRoute(sessionID uint64, lane *laneRuntime, leg transport.LegRef) time.Duration {
	if leg.Kind != transport.KindTCP {
		return l.probeTimeout
	}
	timeoutMS := uint32(tcpHelloInitialTimeoutMS)
	ok := false
	if lane != nil {
		tcpQ := lane.quality.TCP(false)
		if tcpQ.SmoothedRTT > 0 {
			srttMS := uint32(tcpQ.SmoothedRTT / time.Millisecond)
			rttvarMS := uint32(tcpQ.RTTVariance / time.Millisecond)
			timeoutMS = helloRTOMs(srttMS, rttvarMS)
			ok = true
		}
	}
	if !ok {
		if srttMS, sampleOK := l.sessionMaxRTTMs(sessionID); sampleOK {
			timeoutMS = helloRTOMs(srttMS, 0)
		}
	}
	timeoutMS = clampUint32(timeoutMS, tcpHelloMinTimeoutMS, tcpHelloMaxTimeoutMS)
	return time.Duration(timeoutMS) * time.Millisecond
}

func helloRTOMs(srttMS uint32, rttvarMS uint32) uint32 {
	variance64 := uint64(rttvarMS) * 4
	if variance64 < helloRTOGranularityMS {
		variance64 = helloRTOGranularityMS
	}
	raw := uint64(srttMS) + variance64
	if raw > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(raw)
}
