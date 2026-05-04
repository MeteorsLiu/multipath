package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/protocol"
)

const (
	maxFallbackDialTimeout     = time.Second
	fallbackDialInitialBackoff = 5 * time.Second
	fallbackDialMaxBackoff     = 30 * time.Second
)

type legController struct{}

func (legController) fallbackEnabled(sessionOK bool, caps uint16) bool {
	return sessionOK && caps&protocol.CapTCPFallback != 0
}

func (legController) tryStartFallback(lane *laneRuntime, streamAvailable bool, now time.Time) (string, bool) {
	if lane == nil {
		return "", false
	}
	_, udpQ, _, tcpQ := lane.legQualities()
	return lane.tryStartFallback(streamAvailable, now, warmFallbackWanted(udpQ, tcpQ))
}

func (legController) fallbackDialTimeout(probeTimeout time.Duration) time.Duration {
	if probeTimeout > 0 && probeTimeout < maxFallbackDialTimeout {
		return probeTimeout
	}
	return maxFallbackDialTimeout
}

func warmFallbackWanted(udp, tcp LegQuality) bool {
	if !udp.Active || tcp.Active {
		return false
	}
	if udp.DeliveryRate < minUDPDelivery {
		return true
	}
	return udp.SmoothedRTT > 0 &&
		udp.RTTVariance > 0 &&
		udp.RTTVariance >= udp.SmoothedRTT
}
