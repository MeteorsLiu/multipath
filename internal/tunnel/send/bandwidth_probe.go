package send

import (
	"context"
	"math/bits"
	"slices"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	bandwidthProbeTimeout        = time.Second
	bandwidthProbeWindow         = 10 * time.Second
	bandwidthProbeRoundWindow    = 200 * time.Millisecond
	bandwidthProbeUDPPayloadSize = 1200
	bandwidthProbeTCPPayloadSize = 32 * 1024
	bandwidthProbeMinRateBps     = uint64(1_000_000)
	bandwidthProbeMaxFrames      = 64
)

type bandwidthLegState struct {
	key           laneKey
	nextRateBps   uint64
	ewmaBps       uint64
	inFlight      bool
	complete      bool
	startedAt     time.Time
	sampleCount   uint32
	sentFrames    uint64
	ackedFrames   uint64
	ackedBytes    uint64
	lastLoss      float64
	lastRoundLoss float64
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
	done         chan struct{}
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

	slices.SortFunc(lanes, func(a, b laneCandidate) int {
		return int(a.key.laneID) - int(b.key.laneID)
	})

	selectedLane, ok := l.activeBandwidthLane(sessionID)
	if !ok {
		for _, item := range lanes {
			udpLeg, udpQ, tcpLeg, tcpQ := item.lane.legQualities()
			if l.bandwidthProbeNeeded(udpLeg, udpQ) || l.bandwidthProbeNeeded(tcpLeg, tcpQ) {
				selectedLane = item.key
				break
			}
		}
	}
	if selectedLane.sessionID == 0 {
		return
	}

	for _, item := range lanes {
		if item.key != selectedLane {
			continue
		}
		lane := item.lane
		udpLeg, udpQ, tcpLeg, tcpQ := lane.legQualities()
		if l.bandwidthProbeNeeded(udpLeg, udpQ) {
			candidates = append(candidates, candidate{key: item.key, leg: udpLeg})
		}
		if l.bandwidthProbeNeeded(tcpLeg, tcpQ) {
			candidates = append(candidates, candidate{key: item.key, leg: tcpLeg})
		}
		break
	}

	for _, item := range candidates {
		l.maybeStartBandwidthProbe(ctx, item.key, item.leg, now)
	}
}

func (l *Send) activeBandwidthLane(sessionID uint64) (laneKey, bool) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	var selected laneKey
	for _, state := range l.bandwidthLegs {
		if state == nil || !state.inFlight || state.complete || state.key.sessionID != sessionID {
			continue
		}
		if selected.sessionID == 0 || state.key.laneID < selected.laneID {
			selected = state.key
		}
	}
	if selected.sessionID == 0 {
		return laneKey{}, false
	}
	return selected, true
}

func (l *Send) bandwidthProbeNeeded(leg transport.LegRef, quality LegQuality) bool {
	if !quality.Active {
		return false
	}
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return false
	}
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	needed := state == nil || !state.complete
	l.bandwidthMu.Unlock()
	return needed
}

func (l *Send) maybeStartBandwidthProbe(ctx context.Context, key laneKey, leg transport.LegRef, now time.Time) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}

	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		state = &bandwidthLegState{key: key, nextRateBps: bandwidthProbeMinRateBps}
		l.bandwidthLegs[legKey] = state
	}
	state.key = key
	if state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return
	}
	state.inFlight = true
	state.startedAt = now
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "train_start session=%d lane=%d leg={%s} window=%s", key.sessionID, key.laneID, debugLeg(leg), bandwidthProbeWindow)
	go l.runBandwidthProbeTrain(ctx, key, leg, legKey)
}

func probeFrameCount(rateBps uint64, payloadBytes int) uint16 {
	bytesPerRound := rateBps * uint64(bandwidthProbeRoundWindow) / uint64(time.Second) / 8
	count := bytesPerRound / uint64(payloadBytes)
	if count < 2 {
		count = 2
	}
	if count > bandwidthProbeMaxFrames {
		count = bandwidthProbeMaxFrames
	}
	return uint16(count)
}

func (l *Send) runBandwidthProbeTrain(ctx context.Context, key laneKey, leg transport.LegRef, legKey pingKey) {
	for {
		round := l.startBandwidthProbeRound(key, leg, legKey, time.Now())
		if round == nil {
			return
		}
		l.runBandwidthProbeRound(ctx, round)
		timer := time.NewTimer(bandwidthProbeTimeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			l.abortBandwidthProbeTrain(legKey)
			return
		case <-round.done:
			timer.Stop()
		case <-timer.C:
		}
		if l.finishBandwidthProbe(round.probeID) {
			return
		}
	}
}

func (l *Send) startBandwidthProbeRound(key laneKey, leg transport.LegRef, legKey pingKey, now time.Time) *bandwidthProbeRound {
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil || !state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return nil
	}
	rateBps := state.nextRateBps
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	payloadBytes := bandwidthProbeUDPPayloadSize
	if leg.Kind == transport.KindTCP {
		payloadBytes = bandwidthProbeTCPPayloadSize
	}
	probeID := l.nextBWProbeID.Add(1)
	round := &bandwidthProbeRound{
		key:          key,
		leg:          leg,
		legKey:       legKey,
		probeID:      probeID,
		count:        probeFrameCount(rateBps, payloadBytes),
		payloadBytes: payloadBytes,
		rateBps:      rateBps,
		startedAt:    now,
		done:         make(chan struct{}),
	}
	l.bandwidthPending[probeID] = round
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "round_start session=%d lane=%d leg={%s} probe_id=%d rate_bps=%d count=%d payload=%d", key.sessionID, key.laneID, debugLeg(leg), probeID, rateBps, round.count, payloadBytes)
	return round
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
	full := rx.count == 64 && rx.received == ^uint64(0) || rx.count < 64 && rx.received == (uint64(1)<<rx.count)-1
	ack := protocol.BandwidthProbeAckBody{
		ProbeID:   body.ProbeID,
		Count:     rx.count,
		Received:  rx.received,
		FirstRXMS: rx.firstRXMS,
		LastRXMS:  rx.lastRXMS,
	}
	if full {
		delete(l.bandwidthRX, rxKey)
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

func (l *Send) finishBandwidthProbe(probeID uint64) bool {
	l.bandwidthMu.Lock()
	round := l.bandwidthPending[probeID]
	if round == nil {
		l.bandwidthMu.Unlock()
		return false
	}
	delete(l.bandwidthPending, probeID)
	if round.done != nil {
		close(round.done)
	}
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
	now := time.Now()
	if state.startedAt.IsZero() {
		state.startedAt = round.startedAt
	}
	state.sentFrames += uint64(round.count)
	state.ackedFrames += uint64(acked)
	state.ackedBytes += uint64(acked) * uint64(round.payloadBytes)
	sampleBps := bandwidthWindowSampleBps(state.ackedBytes, state.startedAt, now)
	state.ewmaBps = sampleBps
	state.lastRoundLoss = loss
	state.lastLoss = bandwidthAggregateLoss(state.sentFrames, state.ackedFrames)
	state.sampleCount++
	if loss < 0.05 {
		next := state.nextRateBps * 2
		if next < state.nextRateBps {
			next = state.nextRateBps
		}
		if next < bandwidthProbeMinRateBps {
			next = bandwidthProbeMinRateBps
		}
		state.nextRateBps = next
	} else {
		next := state.nextRateBps / 2
		if next < bandwidthProbeMinRateBps {
			next = bandwidthProbeMinRateBps
		}
		state.nextRateBps = next
	}
	if now.Sub(state.startedAt) >= bandwidthProbeWindow {
		state.complete = true
		state.inFlight = false
	}
	ewmaBps := state.ewmaBps
	nextRateBps := state.nextRateBps
	complete := state.complete
	sampleCount := state.sampleCount
	aggregateLoss := state.lastLoss
	l.bandwidthMu.Unlock()

	if complete {
		if lane := l.getLane(round.key); lane != nil {
			lane.recordBandwidthSample(round.leg.Kind, ewmaBps, aggregateLoss)
		}
		metrics.SetGauge(metrics.LaneBandwidthBps, float64(ewmaBps),
			metrics.L("session", round.key.sessionID),
			metrics.L("lane", round.key.laneID),
			metrics.L("leg", kindMetricLabel(round.leg.Kind)),
		)
		metrics.SetGauge(metrics.LaneProbeLossRatio, aggregateLoss,
			metrics.L("session", round.key.sessionID),
			metrics.L("lane", round.key.laneID),
			metrics.L("leg", kindMetricLabel(round.leg.Kind)),
		)
	}
	metrics.IncCounter(metrics.BandwidthProbeEventsTotal,
		metrics.L("event", "finish"),
		metrics.L("session", round.key.sessionID),
		metrics.L("lane", round.key.laneID),
		metrics.L("leg", kindMetricLabel(round.leg.Kind)),
	)
	if complete {
		l.logBandwidthProbeDecisionIfReady(round.key)
	}
	debuglog.Printf("send/bw_probe", "round_finish session=%d lane=%d leg={%s} probe_id=%d acked=%d count=%d round_loss=%.3f window_loss=%.3f window_bps=%d next_rate_bps=%d samples=%d complete=%t", round.key.sessionID, round.key.laneID, debugLeg(round.leg), probeID, acked, round.count, loss, aggregateLoss, ewmaBps, nextRateBps, sampleCount, complete)
	return complete
}

func (l *Send) abortBandwidthProbeTrain(legKey pingKey) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && !state.complete {
		state.inFlight = false
	}
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
			if round.done != nil {
				close(round.done)
			}
		}
	}
	l.bandwidthMu.Unlock()
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
			if round.done != nil {
				close(round.done)
			}
		}
	}
	for key := range l.bandwidthRX {
		if key.legKey == legKey {
			delete(l.bandwidthRX, key)
		}
	}
	l.bandwidthMu.Unlock()
}

func bandwidthWindowSampleBps(ackedBytes uint64, startedAt time.Time, now time.Time) uint64 {
	if ackedBytes == 0 || startedAt.IsZero() {
		return 0
	}
	elapsed := now.Sub(startedAt)
	if elapsed <= 0 {
		return 0
	}
	return ackedBytes * 8 * uint64(time.Second) / uint64(elapsed)
}

func bandwidthAggregateLoss(sentFrames, ackedFrames uint64) float64 {
	if sentFrames == 0 {
		return 1
	}
	if ackedFrames > sentFrames {
		ackedFrames = sentFrames
	}
	return float64(sentFrames-ackedFrames) / float64(sentFrames)
}
