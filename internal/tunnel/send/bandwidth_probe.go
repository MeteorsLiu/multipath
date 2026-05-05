package send

import (
	"context"
	"math/bits"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	bandwidthProbeTimeout     = time.Second
	bandwidthProbeRoundWindow = 200 * time.Millisecond
	bandwidthProbePayloadSize = 1200
	bandwidthProbeMinRateBps  = uint64(1_000_000)
	bandwidthProbeMaxFrames   = 64
	bandwidthProbePlateauGain = 10
	bandwidthProbePlateauNeed = 2
)

type bandwidthLegState struct {
	nextRateBps  uint64
	ewmaBps      uint64
	bestSample   uint64
	inFlight     bool
	complete     bool
	plateauCount uint8
	lastLoss     float64
}

type bandwidthProbeRound struct {
	key          laneKey
	leg          transport.LegRef
	legKey       pingKey
	probeID      uint64
	count        uint16
	payloadBytes int
	rateBps      uint64
	startedAt    time.Time
	received     uint64
	firstRXMS    uint64
	lastRXMS     uint64
}

type bandwidthRXKey struct {
	legKey  pingKey
	probeID uint64
}

type bandwidthRXRound struct {
	count     uint16
	received  uint64
	firstRXMS uint64
	lastRXMS  uint64
}

func (l *Send) probeBandwidth(ctx context.Context, now time.Time) {
	sessionID, ok := l.activeSession()
	if !ok {
		return
	}

	type laneCandidate struct {
		key  laneKey
		lane *laneRuntime
	}
	type candidate struct {
		key laneKey
		leg transport.LegRef
	}
	var lanes []laneCandidate
	var candidates []candidate
	l.lanesMu.RLock()
	for key, lane := range l.lanes {
		if key.sessionID == sessionID {
			lanes = append(lanes, laneCandidate{key: key, lane: lane})
		}
	}
	l.lanesMu.RUnlock()

	for _, item := range lanes {
		key := item.key
		lane := item.lane
		udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
		if udpQ.Active {
			candidates = append(candidates, candidate{key: key, leg: udpLeg})
		}
		if tcpQ.Active {
			candidates = append(candidates, candidate{key: key, leg: tcpLeg})
		}
	}

	for _, item := range candidates {
		l.maybeStartBandwidthProbe(ctx, item.key, item.leg, now)
	}
}

func (l *Send) maybeStartBandwidthProbe(ctx context.Context, key laneKey, leg transport.LegRef, now time.Time) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}

	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		state = &bandwidthLegState{nextRateBps: bandwidthProbeMinRateBps}
		l.bandwidthLegs[legKey] = state
	}
	if state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return
	}
	probeID := l.nextBWProbeID.Add(1)
	rateBps := state.nextRateBps
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	count := probeFrameCount(rateBps)
	round := &bandwidthProbeRound{
		key:          key,
		leg:          leg,
		legKey:       legKey,
		probeID:      probeID,
		count:        count,
		payloadBytes: bandwidthProbePayloadSize,
		rateBps:      rateBps,
		startedAt:    now,
	}
	state.inFlight = true
	l.bandwidthPending[probeID] = round
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "start session=%d lane=%d leg={%s} probe_id=%d rate_bps=%d count=%d payload=%d", key.sessionID, key.laneID, debugLeg(leg), probeID, rateBps, count, bandwidthProbePayloadSize)
	go l.runBandwidthProbeRound(ctx, round)
}

func probeFrameCount(rateBps uint64) uint16 {
	bytesPerRound := rateBps * uint64(bandwidthProbeRoundWindow) / uint64(time.Second) / 8
	count := bytesPerRound / bandwidthProbePayloadSize
	if count < 2 {
		count = 2
	}
	if count > bandwidthProbeMaxFrames {
		count = bandwidthProbeMaxFrames
	}
	return uint16(count)
}

func (l *Send) runBandwidthProbeRound(ctx context.Context, round *bandwidthProbeRound) {
	payload := make([]byte, round.payloadBytes)
	interval := probeFrameInterval(round.rateBps, round.payloadBytes)
	for seq := uint16(0); seq < round.count; seq++ {
		if seq > 0 && interval > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				l.finishBandwidthProbe(round.probeID)
				return
			case <-timer.C:
			}
		}
		nowMS := uint64(time.Now().UnixMilli())
		frame := protocol.Frame{
			Type:      protocol.TypeBandwidthProbe,
			SessionID: round.key.sessionID,
			LaneID:    round.key.laneID,
			Body: protocol.BandwidthProbeBody{
				ProbeID: round.probeID,
				Seq:     seq,
				Count:   round.count,
				SendMS:  nowMS,
				Payload: payload,
			},
		}
		if err := l.writeControlFrameOnLeg(ctx, round.leg, frame); err != nil {
			debuglog.Printf("send/bw_probe", "send_err session=%d lane=%d leg={%s} probe_id=%d seq=%d err=%v", round.key.sessionID, round.key.laneID, debugLeg(round.leg), round.probeID, seq, err)
			break
		}
	}

	timer := time.NewTimer(bandwidthProbeTimeout)
	select {
	case <-ctx.Done():
		timer.Stop()
	case <-timer.C:
	}
	l.finishBandwidthProbe(round.probeID)
}

func probeFrameInterval(rateBps uint64, payloadBytes int) time.Duration {
	if rateBps == 0 || payloadBytes <= 0 {
		return 0
	}
	nanos := uint64(payloadBytes) * 8 * uint64(time.Second) / rateBps
	if nanos == 0 {
		return 0
	}
	return time.Duration(nanos)
}

func (l *Send) receiveBandwidthProbe(ctx context.Context, sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.BandwidthProbeBody) error {
	if _, _, ok := l.getSessionState(sessionID); !ok {
		debuglog.Printf("send/bw_probe", "probe_drop missing_session session=%d lane=%d probe_id=%d leg={%s}", sessionID, laneID, body.ProbeID, debugLeg(leg))
		return l.writeUnknownSessionClose(ctx, sessionID, laneID, leg)
	}
	if lane := l.getLane(laneKey{sessionID: sessionID, laneID: laneID}); lane == nil {
		debuglog.Printf("send/bw_probe", "probe_drop missing_lane session=%d lane=%d probe_id=%d leg={%s}", sessionID, laneID, body.ProbeID, debugLeg(leg))
		return nil
	}
	if body.Count == 0 || body.Count > 64 || body.Seq >= body.Count {
		debuglog.Printf("send/bw_probe", "probe_drop invalid session=%d lane=%d probe_id=%d seq=%d count=%d", sessionID, laneID, body.ProbeID, body.Seq, body.Count)
		return nil
	}

	nowMS := uint64(time.Now().UnixMilli())
	rxKey := bandwidthRXKey{legKey: newPingKey(leg), probeID: body.ProbeID}
	l.bandwidthMu.Lock()
	rx := l.bandwidthRX[rxKey]
	if rx == nil || rx.count != body.Count {
		rx = &bandwidthRXRound{count: body.Count, firstRXMS: nowMS}
		l.bandwidthRX[rxKey] = rx
	}
	rx.received |= uint64(1) << body.Seq
	if rx.firstRXMS == 0 || nowMS < rx.firstRXMS {
		rx.firstRXMS = nowMS
	}
	if nowMS > rx.lastRXMS {
		rx.lastRXMS = nowMS
	}
	ack := protocol.BandwidthProbeAckBody{
		ProbeID:   body.ProbeID,
		Count:     rx.count,
		Received:  rx.received,
		FirstRXMS: rx.firstRXMS,
		LastRXMS:  rx.lastRXMS,
	}
	l.bandwidthMu.Unlock()

	return l.writeControlFrameOnLeg(ctx, leg, protocol.Frame{
		Type:      protocol.TypeBandwidthProbeAck,
		SessionID: sessionID,
		LaneID:    laneID,
		Body:      ack,
	})
}

func (l *Send) receiveBandwidthProbeAck(sessionID uint64, laneID uint8, leg transport.LegRef, body protocol.BandwidthProbeAckBody) error {
	legKey := newPingKey(leg)
	l.bandwidthMu.Lock()
	round := l.bandwidthPending[body.ProbeID]
	if round == nil || round.key.sessionID != sessionID || round.key.laneID != laneID || round.legKey != legKey || round.count != body.Count {
		l.bandwidthMu.Unlock()
		debuglog.Printf("send/bw_probe", "ack_drop stale session=%d lane=%d leg={%s} probe_id=%d count=%d", sessionID, laneID, debugLeg(leg), body.ProbeID, body.Count)
		return nil
	}
	round.received |= body.Received
	if body.FirstRXMS != 0 && (round.firstRXMS == 0 || body.FirstRXMS < round.firstRXMS) {
		round.firstRXMS = body.FirstRXMS
	}
	if body.LastRXMS > round.lastRXMS {
		round.lastRXMS = body.LastRXMS
	}
	received := round.received
	var full uint64
	if round.count == 64 {
		full = ^uint64(0)
	} else {
		full = (uint64(1) << round.count) - 1
	}
	l.bandwidthMu.Unlock()
	if received == full {
		l.finishBandwidthProbe(body.ProbeID)
	}
	return nil
}

func (l *Send) finishBandwidthProbe(probeID uint64) {
	l.bandwidthMu.Lock()
	round := l.bandwidthPending[probeID]
	if round == nil {
		l.bandwidthMu.Unlock()
		return
	}
	delete(l.bandwidthPending, probeID)
	state := l.bandwidthLegs[round.legKey]
	if state == nil {
		state = &bandwidthLegState{}
		l.bandwidthLegs[round.legKey] = state
	}
	acked := bits.OnesCount64(round.received)
	loss := 1.0
	if round.count > 0 {
		loss = float64(int(round.count)-acked) / float64(round.count)
	}
	sampleBps := bandwidthSampleBps(acked, round.payloadBytes, round.firstRXMS, round.lastRXMS)
	state.ewmaBps = ewmaBandwidth(state.ewmaBps, sampleBps)
	state.lastLoss = loss
	state.inFlight = false
	if acked > 0 && loss < 0.05 {
		if samplePlateau(state.bestSample, sampleBps) {
			state.plateauCount++
			if sampleBps > state.bestSample {
				state.bestSample = sampleBps
			}
		} else {
			state.bestSample = sampleBps
			state.plateauCount = 0
		}
		if state.plateauCount >= bandwidthProbePlateauNeed {
			state.complete = true
		} else {
			next := state.nextRateBps * 2
			if next < state.nextRateBps {
				state.complete = true
			} else if next < bandwidthProbeMinRateBps {
				next = bandwidthProbeMinRateBps
				state.nextRateBps = next
			} else {
				state.nextRateBps = next
			}
		}
	} else {
		next := state.nextRateBps / 2
		if next < bandwidthProbeMinRateBps {
			next = bandwidthProbeMinRateBps
		}
		state.nextRateBps = next
		state.complete = true
	}
	ewmaBps := state.ewmaBps
	nextRateBps := state.nextRateBps
	complete := state.complete
	l.bandwidthMu.Unlock()

	if lane := l.getLane(round.key); lane != nil {
		lane.recordBandwidthSample(round.leg.Kind, ewmaBps, loss)
	}
	metrics.SetGauge(metrics.LaneBandwidthBps, float64(ewmaBps),
		metrics.L("session", round.key.sessionID),
		metrics.L("lane", round.key.laneID),
		metrics.L("leg", kindMetricLabel(round.leg.Kind)),
	)
	metrics.SetGauge(metrics.LaneProbeLossRatio, loss,
		metrics.L("session", round.key.sessionID),
		metrics.L("lane", round.key.laneID),
		metrics.L("leg", kindMetricLabel(round.leg.Kind)),
	)
	metrics.IncCounter(metrics.BandwidthProbeEventsTotal,
		metrics.L("event", "finish"),
		metrics.L("session", round.key.sessionID),
		metrics.L("lane", round.key.laneID),
		metrics.L("leg", kindMetricLabel(round.leg.Kind)),
	)
	if complete {
		l.logBandwidthProbeDecisionIfReady(round.key)
	}
	debuglog.Printf("send/bw_probe", "finish session=%d lane=%d leg={%s} probe_id=%d acked=%d count=%d loss=%.3f sample_bps=%d ewma_bps=%d next_rate_bps=%d complete=%t", round.key.sessionID, round.key.laneID, debugLeg(round.leg), probeID, acked, round.count, loss, sampleBps, ewmaBps, nextRateBps, complete)
}

func (l *Send) logBandwidthProbeDecisionIfReady(key laneKey) {
	lane := l.getLane(key)
	if lane == nil {
		return
	}
	udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
	if udpQ.ProbeSamples < minBandwidthProbeSamples || tcpQ.ProbeSamples < minBandwidthProbeSamples {
		return
	}
	udpKey := newPingKey(udpLeg)
	tcpKey := newPingKey(tcpLeg)
	if udpKey.kind == 0 || tcpKey.kind == 0 {
		return
	}
	l.bandwidthMu.Lock()
	udpState := l.bandwidthLegs[udpKey]
	tcpState := l.bandwidthLegs[tcpKey]
	ready := udpState != nil && udpState.complete && tcpState != nil && tcpState.complete
	l.bandwidthMu.Unlock()
	if !ready {
		return
	}
	logBandwidthProbeDecision(key.sessionID, key.laneID, udpQ, tcpQ)
}

func (l *Send) clearBandwidthLeg(leg transport.LegRef) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}
	l.bandwidthMu.Lock()
	delete(l.bandwidthLegs, legKey)
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
		}
	}
	for key := range l.bandwidthRX {
		if key.legKey == legKey {
			delete(l.bandwidthRX, key)
		}
	}
	l.bandwidthMu.Unlock()
}

func bandwidthSampleBps(acked int, payloadBytes int, firstRXMS, lastRXMS uint64) uint64 {
	if acked <= 0 || payloadBytes <= 0 {
		return 0
	}
	spanMS := uint64(1)
	if lastRXMS > firstRXMS {
		spanMS = lastRXMS - firstRXMS
	}
	return uint64(acked) * uint64(payloadBytes) * 8 * 1000 / spanMS
}

func ewmaBandwidth(old, sample uint64) uint64 {
	if old == 0 {
		return sample
	}
	return (old*7 + sample) / 8
}

func samplePlateau(best, sample uint64) bool {
	if best == 0 {
		return false
	}
	return sample <= best+best*bandwidthProbePlateauGain/100
}
