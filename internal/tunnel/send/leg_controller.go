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
	return lane.tryStartFallback(streamAvailable, now, tcpWarmWanted(udpQ, tcpQ))
}

func (legController) fallbackDialTimeout(probeTimeout time.Duration) time.Duration {
	if probeTimeout > 0 && probeTimeout < maxFallbackDialTimeout {
		return probeTimeout
	}
	return maxFallbackDialTimeout
}

func tcpWarmWanted(udp, tcp LegQuality) bool {
	return udp.Active && !tcp.Active
}
