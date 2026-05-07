package send

import (
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
	"github.com/MeteorsLiu/multipath/internal/tunnel/send/rtt"
)

type rttPendingKey struct {
	target probe.Target
	pingID uint64
}

type rttPendingPing struct {
	sessionID  uint64
	laneID     uint8
	legKey     pingKey
	timeMS     uint64
	deadlineMS uint64
}

type rttObservation struct {
	sampleMS uint32
	srttMS   uint32
	rttvarMS uint32
	samples  uint32
}

func (l *Send) recordRTTPing(target probe.Target, pingID uint64, timeMS uint64, binding probeBinding) {
	if target == 0 {
		return
	}

	// Compute delivery deadline: 2x SRTT, floor 100ms.
	var deadlineMS uint64
	lane := l.getLane(laneKey{sessionID: binding.sessionID, laneID: binding.laneID})
	if lane != nil {
		lane.mu.Lock()
		estimator := laneRTTEstimatorLocked(lane, binding.leg.Kind)
		if estimator != nil {
			if srtt, ok := estimator.SRTT(); ok && srtt > 0 {
				d := uint64(srtt) * 2
				if d < 100 {
					d = 100
				}
				deadlineMS = d
			}
		}
		lane.mu.Unlock()
	}

	l.rttMu.Lock()
	if l.rttPending == nil {
		l.rttPending = make(map[rttPendingKey]rttPendingPing)
	}
	l.pruneRTTPendingLocked(target, timeMS)
	l.rttPending[rttPendingKey{target: target, pingID: pingID}] = rttPendingPing{
		sessionID:  binding.sessionID,
		laneID:     binding.laneID,
		legKey:     newPingKey(binding.leg),
		timeMS:     timeMS,
		deadlineMS: deadlineMS,
	}
	l.rttMu.Unlock()
}

func (l *Send) acceptRTTPong(lane *laneRuntime, sessionID uint64, laneID uint8, leg transport.LegRef, target probe.Target, body protocol.PingBody, nowMS uint64) (rttObservation, bool) {
	if lane == nil || target == 0 {
		return rttObservation{}, false
	}

	key := rttPendingKey{target: target, pingID: body.PingID}
	legKey := newPingKey(leg)

	l.rttMu.Lock()
	pending, ok := l.rttPending[key]
	l.rttMu.Unlock()

	if !ok ||
		pending.sessionID != sessionID ||
		pending.laneID != laneID ||
		pending.legKey != legKey ||
		pending.timeMS != body.TimeMS ||
		nowMS < body.TimeMS {
		return rttObservation{}, false
	}

	l.rttMu.Lock()
	if current, ok := l.rttPending[key]; ok && current == pending {
		delete(l.rttPending, key)
	}
	l.rttMu.Unlock()

	sample64 := nowMS - body.TimeMS
	sampleMS := uint32(sample64)
	if sample64 > uint64(^uint32(0)) {
		sampleMS = ^uint32(0)
	}

	lane.mu.Lock()
	estimator := laneRTTEstimatorLocked(lane, leg.Kind)
	if estimator == nil {
		lane.mu.Unlock()
		return rttObservation{}, false
	}
	estimator.Add(sampleMS)
	srttMS, _ := estimator.SRTT()
	rttvarMS, _ := estimator.RTTVAR()
	samples := estimator.Samples()
	lane.mu.Unlock()

	metrics.SetGauge(metrics.LaneRTTMs, float64(srttMS),
		metrics.L("session", sessionID),
		metrics.L("lane", laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)

	return rttObservation{
		sampleMS: sampleMS,
		srttMS:   srttMS,
		rttvarMS: rttvarMS,
		samples:  samples,
	}, true
}

func (l *Send) clearRTTPendingTarget(target probe.Target) {
	if target == 0 {
		return
	}
	l.rttMu.Lock()
	for key := range l.rttPending {
		if key.target == target {
			delete(l.rttPending, key)
		}
	}
	l.rttMu.Unlock()
}

func (l *Send) pruneRTTPendingLocked(target probe.Target, nowMS uint64) {
	if l.probeTimeout <= 0 {
		return
	}
	timeoutMS := uint64(l.probeTimeout.Milliseconds())
	if timeoutMS == 0 {
		timeoutMS = 1
	}
	for key, pending := range l.rttPending {
		if key.target != target || pending.timeMS > nowMS {
			continue
		}
		if nowMS-pending.timeMS >= timeoutMS {
			delete(l.rttPending, key)
		}
	}
}

func (l *Send) sessionMaxRTTMs(sessionID uint64) (uint32, bool) {
	var max uint32
	ok := false
	for _, lane := range l.runnableLanes(sessionID) {
		lane.mu.Lock()
		srtt, sampleOK := laneActiveLegMaxRTTLocked(lane)
		lane.mu.Unlock()
		if !sampleOK {
			continue
		}
		if !ok || srtt > max {
			max = srtt
			ok = true
		}
	}
	return max, ok
}

func (l *Send) sessionMinRTTMs(sessionID uint64) (uint32, bool) {
	var min uint32
	ok := false
	for _, lane := range l.runnableLanes(sessionID) {
		lane.mu.Lock()
		srtt, sampleOK := laneActiveLegMaxRTTLocked(lane)
		lane.mu.Unlock()
		if !sampleOK {
			continue
		}
		if !ok || srtt < min {
			min = srtt
			ok = true
		}
	}
	return min, ok
}

func laneActiveLegMaxRTTLocked(lane *laneRuntime) (uint32, bool) {
	var max uint32
	var ok bool
	if lane.udpReady && lane.udpLeg.EndpointID != "" && lane.udpLeg.RemoteAddr != nil {
		if srtt, sampleOK := lane.rttUDP.SRTT(); sampleOK {
			max, ok = srtt, true
		}
	}
	if lane.tcpReady && lane.tcpLeg.ConnID != "" {
		if srtt, sampleOK := lane.rttTCP.SRTT(); sampleOK && (!ok || srtt > max) {
			max, ok = srtt, true
		}
	}
	return max, ok
}

func laneSRTTLocked(lane *laneRuntime, kind transport.Kind) (uint32, bool) {
	estimator := laneRTTEstimatorLocked(lane, kind)
	if estimator == nil {
		return 0, false
	}
	return estimator.SRTT()
}

func laneRTTEstimatorLocked(lane *laneRuntime, kind transport.Kind) *rtt.Estimator {
	if lane == nil {
		return nil
	}
	switch kind {
	case transport.KindUDP:
		return &lane.rttUDP
	case transport.KindTCP:
		return &lane.rttTCP
	default:
		return nil
	}
}
