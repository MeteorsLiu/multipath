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
	if srttMS, rttvarMS, ok := laneRTTLocked(lane, transport.KindTCP); ok {
		timeoutMS = helloRTOMs(srttMS, rttvarMS)
	} else if srttMS, ok := l.sessionMaxRTTMs(sessionID); ok {
		timeoutMS = helloRTOMs(srttMS, 0)
	}
	timeoutMS = clampUint32(timeoutMS, tcpHelloMinTimeoutMS, tcpHelloMaxTimeoutMS)
	return time.Duration(timeoutMS) * time.Millisecond
}

func laneRTTLocked(lane *laneRuntime, kind transport.Kind) (uint32, uint32, bool) {
	if lane == nil {
		return 0, 0, false
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	estimator := laneRTTEstimatorLocked(lane, kind)
	if estimator == nil {
		return 0, 0, false
	}
	srttMS, ok := estimator.SRTT()
	if !ok {
		return 0, 0, false
	}
	rttvarMS, _ := estimator.RTTVAR()
	return srttMS, rttvarMS, true
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
