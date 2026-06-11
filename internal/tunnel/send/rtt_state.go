package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
	legpkg "github.com/MeteorsLiu/multipath/internal/tunnel/send/leg"
)

func (l *Send) recordRTTPing(target probe.Target, pingID uint64, timeMS uint64, binding probeBinding) {
	if target == 0 {
		return
	}

	lane := l.getLane(laneKey{sessionID: binding.sessionID, laneID: binding.laneID})
	if lane != nil {
		lane.quality.OnPingSent(binding.leg.Kind, inflightKey(target, pingID, binding), timeMS, rttPruneTimeoutMS(l.probeTimeout))
	}
}

func (l *Send) acceptRTTPong(lane *laneRuntime, sessionID uint64, laneID uint8, leg transport.LegRef, target probe.Target, body protocol.PingBody, nowMS uint64) (legpkg.RTTSample, uint64, bool) {
	if lane == nil || target == 0 {
		return legpkg.RTTSample{}, 0, false
	}

	result, ok := lane.quality.OnPong(leg.Kind, legpkg.InflightKey{
		Target:    uint64(target),
		PingID:    body.PingID,
		SessionID: sessionID,
		LaneID:    laneID,
		Leg:       probeLegKey(newPingKey(leg)),
	}, body.TimeMS, nowMS)
	if !ok {
		return legpkg.RTTSample{}, 0, false
	}

	metrics.SetGauge(metrics.LaneRTTMs, float64(result.Sample.SRTTMS),
		metrics.L("session", sessionID),
		metrics.L("lane", laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)

	return result.Sample, result.DeadlineMS, true
}

func (l *Send) notifyProbeDown(target probe.Target, binding probeBinding) {
	if target == 0 {
		return
	}
	lane := l.getLane(laneKey{sessionID: binding.sessionID, laneID: binding.laneID})
	if lane == nil {
		return
	}
	lane.quality.OnProbeDown(uint64(target))
}

func inflightKey(target probe.Target, pingID uint64, binding probeBinding) legpkg.InflightKey {
	return legpkg.InflightKey{
		Target:    uint64(target),
		PingID:    pingID,
		SessionID: binding.sessionID,
		LaneID:    binding.laneID,
		Leg:       probeLegKey(newPingKey(binding.leg)),
	}
}

func probeLegKey(key pingKey) legpkg.ProbeLegKey {
	return legpkg.ProbeLegKey{
		Kind:       uint8(key.kind),
		EndpointID: key.endpointID,
		Remote:     key.remote,
		ConnID:     key.connID,
	}
}

func rttPruneTimeoutMS(timeout time.Duration) uint64 {
	if timeout <= 0 {
		return 0
	}
	timeoutMS := uint64(timeout.Milliseconds())
	if timeoutMS == 0 {
		return 1
	}
	return timeoutMS
}

func (l *Send) sessionMaxRTTMs(sessionID uint64) (uint32, bool) {
	var max uint32
	ok := false
	for _, lane := range l.runnableLanes(sessionID) {
		_, udpQ, _, tcpQ := lane.legQualities()
		if udpQ.Active && udpQ.SmoothedRTT > 0 {
			srtt := uint32(udpQ.SmoothedRTT / time.Millisecond)
			if !ok || srtt > max {
				max = srtt
				ok = true
			}
		}
		if tcpQ.Active && tcpQ.SmoothedRTT > 0 {
			srtt := uint32(tcpQ.SmoothedRTT / time.Millisecond)
			if !ok || srtt > max {
				max = srtt
				ok = true
			}
		}
	}
	return max, ok
}

func (l *Send) sessionMinRTTMs(sessionID uint64) (uint32, bool) {
	var min uint32
	ok := false
	for _, lane := range l.runnableLanes(sessionID) {
		_, udpQ, _, tcpQ := lane.legQualities()
		if udpQ.Active && udpQ.SmoothedRTT > 0 {
			srtt := uint32(udpQ.SmoothedRTT / time.Millisecond)
			if !ok || srtt < min {
				min = srtt
				ok = true
			}
		}
		if tcpQ.Active && tcpQ.SmoothedRTT > 0 {
			srtt := uint32(tcpQ.SmoothedRTT / time.Millisecond)
			if !ok || srtt < min {
				min = srtt
				ok = true
			}
		}
	}
	return min, ok
}
