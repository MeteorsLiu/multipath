package send

import (
	"context"
	"encoding/binary"
	"math/bits"
	randv2 "math/rand/v2"
	"slices"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"golang.org/x/time/rate"
)

const (
	bandwidthProbeAckGrace             = 500 * time.Millisecond
	bandwidthProbeWindow               = 10 * time.Second
	bandwidthProbeRoundWindow          = 500 * time.Millisecond
	bandwidthProbeBurstWindow          = 2 * time.Millisecond
	bandwidthProbeUDPMinPayloadSize    = 1200
	bandwidthProbeUDPMaxPayloadSize    = 1400
	bandwidthProbeTCPPayloadSize       = 32 * 1024
	bandwidthProbeMinRateBps           = uint64(16_000_000)
	bandwidthProbeAdditiveStepBps      = uint64(10_000_000)
	bandwidthProbePacingGainNum        = uint64(3)
	bandwidthProbePacingGainDen        = uint64(2)
	bandwidthProbeGrowthMinNum         = uint64(11)
	bandwidthProbeGrowthMinDen         = uint64(10)
	bandwidthProbeDeliveryMinNum       = uint64(99)
	bandwidthProbeDeliveryMinDen       = uint64(100)
	bandwidthProbeAttemptMinNum        = uint64(95)
	bandwidthProbeAttemptMinDen        = uint64(100)
	bandwidthProbeCeilingMinTargetBps  = bandwidthProbeMinRateBps + 6*bandwidthProbeAdditiveStepBps
	bandwidthProbeLossIncreaseEpsilon  = 0.005
	bandwidthProbeStepAckMinNum        = uint64(9)
	bandwidthProbeStepAckMinDen        = uint64(10)
	bandwidthProbeMaxFrames            = 64
	bandwidthProbeMultiplicativeChunks = 0
	bandwidthProbeAckEvery             = 16
	bandwidthProbeFrameOverhead        = 30
)

type bandwidthLegState struct {
	key            laneKey
	nextRateBps    uint64
	ewmaBps        uint64
	inFlight       bool
	complete       bool
	startedAt      time.Time
	endedAt        time.Time
	sampleCount    uint32
	rampChunks     uint32
	sentFrames     uint64
	ackedFrames    uint64
	ackedBytes     uint64
	currentStepID  uint64
	lastStepID     uint64
	stepStartedAt  time.Time
	lastStepBps    uint64
	lastStepBytes  uint64
	prevStepBytes  uint64
	rateCeilingBps uint64
	maxStepBps     uint64
	steps          map[uint64]*bandwidthProbeStep
	lastLoss       float64
	prevStepLoss   float64
	lastStepLoss   float64
	lastRoundLoss  float64
}

type bandwidthProbeStep struct {
	startedAt      time.Time
	endedAt        time.Time
	rateBps        uint64
	prevAckedBytes uint64
	firstRXMS      uint64
	lastRXMS       uint64
	sentBytes      uint64
	ackedBytes     uint64
	sentFrames     uint64
	ackedFrames    uint64
}

type bandwidthProbeRound struct {
	key          laneKey
	leg          transport.LegRef
	legKey       pingKey
	stepID       uint64
	probeID      uint64
	count        uint16
	payloadBytes int
	frameBytes   int
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
	if !l.bandwidthProbe {
		return
	}
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
	if ok {
		return
	}
	for _, item := range lanes {
		udpLeg, udpQ, tcpLeg, tcpQ := item.lane.legQualities()
		if l.bandwidthProbeNeeded(udpLeg, udpQ) || l.bandwidthProbeNeeded(tcpLeg, tcpQ) {
			selectedLane = item.key
			break
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
		} else if l.bandwidthProbeNeeded(tcpLeg, tcpQ) {
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
	if !quality.Active && !bandwidthProbeCanUseInactiveLeg(leg) {
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

func bandwidthProbeCanUseInactiveLeg(leg transport.LegRef) bool {
	return leg.Kind == transport.KindUDP && leg.EndpointID != "" && leg.RemoteAddr != nil
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
	state.endedAt = time.Time{}
	state.sampleCount = 0
	state.rampChunks = 0
	state.sentFrames = 0
	state.ackedFrames = 0
	state.ackedBytes = 0
	state.currentStepID = 0
	state.lastStepID = 0
	state.stepStartedAt = time.Time{}
	state.lastStepBps = 0
	state.lastStepBytes = 0
	state.prevStepBytes = 0
	state.rateCeilingBps = 0
	state.maxStepBps = 0
	state.steps = make(map[uint64]*bandwidthProbeStep)
	state.lastLoss = 0
	state.prevStepLoss = 0
	state.lastStepLoss = 0
	state.lastRoundLoss = 0
	state.ewmaBps = 0
	if state.nextRateBps == 0 {
		state.nextRateBps = bandwidthProbeMinRateBps
	}
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
	deadline := time.Now().Add(bandwidthProbeWindow)
	var limiterState bandwidthProbeLimiterState
	for {
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		stepDeadline := now.Add(bandwidthProbeRoundWindow)
		if stepDeadline.After(deadline) {
			stepDeadline = deadline
		}
		l.startBandwidthProbeStep(legKey, now)
		for time.Now().Before(stepDeadline) {
			round := l.startBandwidthProbeRound(key, leg, legKey, time.Now())
			if round == nil {
				return
			}
			limiter := limiterState.forRound(leg, round)
			sent := l.runBandwidthProbeRound(ctx, round, stepDeadline, limiter)
			l.recordBandwidthProbeSent(round, sent)
			if sent < round.count {
				if ctx.Err() != nil || time.Now().Before(deadline) && time.Now().Before(stepDeadline) {
					l.abortBandwidthProbeTrain(legKey)
					return
				}
				break
			}
		}
		l.finishBandwidthProbeStep(legKey, stepDeadline)
		l.advanceBandwidthProbeRate(legKey)
	}
	l.markBandwidthProbeSendComplete(legKey, deadline)

	timer := time.NewTimer(bandwidthProbeAckGrace)
	select {
	case <-ctx.Done():
		timer.Stop()
		l.abortBandwidthProbeTrain(legKey)
		return
	case <-timer.C:
	}
	l.completeBandwidthProbeTrain(key, leg, legKey)
}

type bandwidthProbeLimiterState struct {
	limiter *rate.Limiter
	rateBps uint64
	burst   int
}

func (s *bandwidthProbeLimiterState) forRound(leg transport.LegRef, round *bandwidthProbeRound) *rate.Limiter {
	if round == nil {
		return nil
	}
	bytesPerSecond := bandwidthProbeLimiterBytesPerSecond(round.rateBps)
	burst := bandwidthProbeLimiterBurst(round.rateBps, bandwidthProbeLimiterPayloadBytes(leg))
	if bytesPerSecond == 0 || burst == 0 {
		return nil
	}
	limit := rate.Limit(bytesPerSecond)
	if s.limiter == nil {
		s.limiter = rate.NewLimiter(limit, burst)
	} else {
		now := time.Now()
		if s.rateBps != round.rateBps {
			s.limiter.SetLimitAt(now, limit)
		}
		if s.burst != burst {
			s.limiter.SetBurstAt(now, burst)
		}
	}
	s.rateBps = round.rateBps
	s.burst = burst
	return s.limiter
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
	probeID := l.nextBWProbeID.Add(1)
	payloadBytes := bandwidthProbeUDPPayloadSize(key, probeID, rateBps)
	if leg.Kind == transport.KindTCP {
		payloadBytes = bandwidthProbeTCPPayloadSize
	}
	frameBytes := bandwidthProbeFrameBytes(payloadBytes)
	stepID := state.currentStepID
	round := &bandwidthProbeRound{
		key:          key,
		leg:          leg,
		legKey:       legKey,
		stepID:       stepID,
		probeID:      probeID,
		count:        probeFrameCount(rateBps, frameBytes),
		payloadBytes: payloadBytes,
		frameBytes:   frameBytes,
		rateBps:      rateBps,
		startedAt:    now,
	}
	l.bandwidthPending[probeID] = round
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "round_start session=%d lane=%d leg={%s} probe_id=%d rate_bps=%d count=%d payload=%d frame_bytes=%d", key.sessionID, key.laneID, debugLeg(leg), probeID, rateBps, round.count, payloadBytes, frameBytes)
	return round
}

func (l *Send) runBandwidthProbeRound(ctx context.Context, round *bandwidthProbeRound, deadline time.Time, limiter *rate.Limiter) uint16 {
	payload := make([]byte, round.payloadBytes)
	fillBandwidthProbePayload(payload, round)
	for seq := uint16(0); seq < round.count; seq++ {
		if !time.Now().Before(deadline) {
			return seq
		}
		if limiter != nil {
			if err := limiter.WaitN(ctx, round.frameBytes); err != nil {
				return seq
			}
		}
		if !time.Now().Before(deadline) {
			return seq
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
			return seq
		}
	}
	return round.count
}

func fillBandwidthProbePayload(payload []byte, round *bandwidthProbeRound) {
	if len(payload) == 0 || round == nil {
		return
	}
	seed := bandwidthProbeSeed(round.key, round.probeID, round.rateBps)
	binary.BigEndian.PutUint32(seed[25:29], uint32(round.payloadBytes))
	rng := randv2.NewChaCha8(seed)
	_, _ = rng.Read(payload)
}

func bandwidthProbeUDPPayloadSize(key laneKey, probeID uint64, rateBps uint64) int {
	const span = bandwidthProbeUDPMaxPayloadSize - bandwidthProbeUDPMinPayloadSize + 1
	seed := bandwidthProbeSeed(key, probeID, rateBps)
	rng := randv2.NewChaCha8(seed)
	return bandwidthProbeUDPMinPayloadSize + int(rng.Uint64()%uint64(span))
}

func bandwidthProbeSeed(key laneKey, probeID uint64, rateBps uint64) [32]byte {
	var seed [32]byte
	binary.BigEndian.PutUint64(seed[0:8], key.sessionID)
	seed[8] = key.laneID
	binary.BigEndian.PutUint64(seed[9:17], probeID)
	binary.BigEndian.PutUint64(seed[17:25], rateBps)
	return seed
}

func bandwidthProbeLimiterPayloadBytes(leg transport.LegRef) int {
	if leg.Kind == transport.KindTCP {
		return bandwidthProbeFrameBytes(bandwidthProbeTCPPayloadSize)
	}
	return bandwidthProbeFrameBytes(bandwidthProbeUDPMaxPayloadSize)
}

func bandwidthProbeFrameBytes(payloadBytes int) int {
	return payloadBytes + bandwidthProbeFrameOverhead
}

func newBandwidthProbeLimiter(rateBps uint64, payloadBytes int) *rate.Limiter {
	bytesPerSecond := bandwidthProbeLimiterBytesPerSecond(rateBps)
	burst := bandwidthProbeLimiterBurst(rateBps, payloadBytes)
	if bytesPerSecond == 0 || burst == 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(bytesPerSecond), burst)
}

func bandwidthProbeLimiterBytesPerSecond(rateBps uint64) uint64 {
	return rateBps / 8
}

func bandwidthProbeLimiterBurst(rateBps uint64, payloadBytes int) int {
	bytesPerSecond := bandwidthProbeLimiterBytesPerSecond(rateBps)
	if bytesPerSecond == 0 || payloadBytes <= 0 {
		return 0
	}
	burst := int(bytesPerSecond * uint64(bandwidthProbeBurstWindow) / uint64(time.Second))
	if burst < payloadBytes {
		burst = payloadBytes
	}
	return burst
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
	shouldAck := full || body.Seq+1 == rx.count || (body.Seq+1)%bandwidthProbeAckEvery == 0
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

	if !shouldAck {
		return nil
	}
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
	prev := round.received
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
	if received == full {
		delete(l.bandwidthPending, body.ProbeID)
	}
	newBits := received &^ prev
	if newBits != 0 {
		if state := l.bandwidthLegs[legKey]; state != nil && !state.complete {
			acked := bits.OnesCount64(newBits)
			ackedBytes := uint64(acked) * uint64(round.frameBytes)
			state.ackedFrames += uint64(acked)
			state.ackedBytes += ackedBytes
			if state.steps == nil {
				state.steps = make(map[uint64]*bandwidthProbeStep)
			}
			step := state.steps[round.stepID]
			if step == nil {
				step = &bandwidthProbeStep{startedAt: round.startedAt}
				state.steps[round.stepID] = step
			}
			if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
				step.startedAt = round.startedAt
			}
			if step.rateBps == 0 {
				step.rateBps = round.rateBps
			}
			updateBandwidthProbeStepRXSpan(step, round)
			step.ackedBytes += ackedBytes
			step.ackedFrames += uint64(acked)
			l.updateBandwidthProbeStepSample(state, legKey, round.stepID, step)
			state.lastRoundLoss = float64(int(round.count)-bits.OnesCount64(received)) / float64(round.count)
			if state.lastRoundLoss < 0 {
				state.lastRoundLoss = 0
			}
		}
	}
	l.bandwidthMu.Unlock()
	return nil
}

func (l *Send) recordBandwidthProbeSent(round *bandwidthProbeRound, sent uint16) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	if sent == 0 {
		delete(l.bandwidthPending, round.probeID)
		return
	}
	state := l.bandwidthLegs[round.legKey]
	if state == nil {
		state = &bandwidthLegState{}
		l.bandwidthLegs[round.legKey] = state
	}
	state.sentFrames += uint64(sent)
	if state.steps == nil {
		state.steps = make(map[uint64]*bandwidthProbeStep)
	}
	step := state.steps[round.stepID]
	if step == nil {
		step = &bandwidthProbeStep{startedAt: round.startedAt}
		state.steps[round.stepID] = step
	}
	if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
		step.startedAt = round.startedAt
	}
	if step.rateBps == 0 {
		step.rateBps = round.rateBps
	}
	step.sentBytes += uint64(sent) * uint64(round.frameBytes)
	step.sentFrames += uint64(sent)
}

func (l *Send) startBandwidthProbeStep(legKey pingKey, now time.Time) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && !state.complete {
		state.currentStepID++
		state.stepStartedAt = now
		if state.steps == nil {
			state.steps = make(map[uint64]*bandwidthProbeStep)
		}
		state.steps[state.currentStepID] = &bandwidthProbeStep{
			startedAt:      now,
			rateBps:        state.nextRateBps,
			prevAckedBytes: state.prevStepBytes,
		}
	}
	l.bandwidthMu.Unlock()
}

func (l *Send) finishBandwidthProbeStep(legKey pingKey, endedAt time.Time) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete || state.stepStartedAt.IsZero() {
		return
	}
	stepID := state.currentStepID
	step := state.steps[stepID]
	ackedBytes := uint64(0)
	if step != nil {
		ackedBytes = step.ackedBytes
		step.endedAt = endedAt
	}
	stepBps := bandwidthWindowSampleBps(ackedBytes, state.stepStartedAt, endedAt)
	state.lastStepBps = stepBps
	state.lastStepBytes = ackedBytes
	state.lastStepLoss = bandwidthAggregateLoss(stepSentFrames(step), stepAckedFrames(step))
	state.lastStepID = stepID
	if stepBps > state.maxStepBps {
		state.maxStepBps = stepBps
	}
	state.stepStartedAt = time.Time{}
}

func (l *Send) updateBandwidthProbeStepSample(state *bandwidthLegState, legKey pingKey, stepID uint64, step *bandwidthProbeStep) {
	if state == nil || step == nil || step.endedAt.IsZero() {
		return
	}
	l.refreshBandwidthProbeStepSample(state, stepID, step)
	l.lockBandwidthProbeRateCeilingFromSteps(state, legKey)
}

func (l *Send) refreshBandwidthProbeStepSample(state *bandwidthLegState, stepID uint64, step *bandwidthProbeStep) {
	stepBps := bandwidthProbeStepCeilingBps(step)
	if stepID != state.lastStepID {
		if stepBps > state.maxStepBps {
			state.maxStepBps = stepBps
		}
		return
	}
	state.lastStepBps = stepBps
	state.lastStepBytes = step.ackedBytes
	state.lastStepLoss = bandwidthAggregateLoss(stepSentFrames(step), stepAckedFrames(step))
	if stepBps > state.maxStepBps {
		state.maxStepBps = stepBps
	}
}

func (l *Send) advanceBandwidthProbeRate(legKey pingKey) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete {
		return
	}
	if state.nextRateBps == 0 {
		state.nextRateBps = bandwidthProbeMinRateBps
	}
	if state.lastStepID == 0 {
		state.lastStepID = state.currentStepID
	}
	if state.rateCeilingBps > 0 {
		state.nextRateBps = state.rateCeilingBps
	} else if l.lockBandwidthProbeRateCeilingFromSteps(state, legKey) {
	} else if state.lastStepBps > 0 {
		next := uint64(0)
		if state.rampChunks < bandwidthProbeMultiplicativeChunks &&
			!bandwidthProbeLossIncreased(state.prevStepLoss, state.lastStepLoss) {
			next = state.lastStepBps * bandwidthProbePacingGainNum / bandwidthProbePacingGainDen
		} else {
			next = state.nextRateBps + bandwidthProbeAdditiveStepBps
		}
		if next < bandwidthProbeMinRateBps {
			next = bandwidthProbeMinRateBps
		}
		state.nextRateBps = next
	} else {
		next := state.nextRateBps + bandwidthProbeAdditiveStepBps
		if next < state.nextRateBps {
			next = state.nextRateBps
		}
		state.nextRateBps = next
	}
	if step := state.steps[state.lastStepID]; step != nil {
		state.prevStepBytes = step.ackedBytes
		state.prevStepLoss = bandwidthAggregateLoss(stepSentFrames(step), stepAckedFrames(step))
	}
	state.rampChunks++
}

func (l *Send) lockBandwidthProbeRateCeilingFromSteps(state *bandwidthLegState, legKey pingKey) bool {
	if state == nil || state.rateCeilingBps > 0 {
		return false
	}
	for stepID := uint64(1); stepID <= state.lastStepID; stepID++ {
		step := state.steps[stepID]
		if !l.lockBandwidthProbeRateCeilingForStep(state, legKey, stepID, step) {
			continue
		}
		if stepID == state.lastStepID {
			state.prevStepBytes = state.lastStepBytes
			state.prevStepLoss = state.lastStepLoss
		}
		return true
	}
	return false
}

func (l *Send) lockBandwidthProbeRateCeilingForStep(state *bandwidthLegState, legKey pingKey, stepID uint64, step *bandwidthProbeStep) bool {
	if step == nil || step.endedAt.IsZero() || step.rateBps == 0 || state.rateCeilingBps > 0 {
		return false
	}
	stepBps := bandwidthProbeStepCeilingBps(step)
	sentBps := bandwidthProbeStepSentBps(step)
	prevAckedBytes := bandwidthProbePreviousStepAckedBytes(state, stepID, step)
	if prevAckedBytes == 0 {
		return false
	}
	if !bandwidthProbeStepAckComplete(step) ||
		!bandwidthProbeTargetAttempted(sentBps, step.rateBps) ||
		!bandwidthProbeCeilingTargetMature(step.rateBps) ||
		!bandwidthProbeUnderDelivered(stepBps, step.rateBps) ||
		!bandwidthProbeGrowthStalled(prevAckedBytes, step.ackedBytes) {
		return false
	}
	state.rateCeilingBps = stepBps
	state.nextRateBps = state.rateCeilingBps
	debuglog.Printf("send/bw_probe", "rate_ceiling session=%d lane=%d kind=%s rate_bps=%d target_bps=%d prev_acked_bytes=%d acked_bytes=%d sent_bps=%d step_id=%d", state.key.sessionID, state.key.laneID, kindMetricLabel(legKey.kind), state.rateCeilingBps, step.rateBps, prevAckedBytes, step.ackedBytes, sentBps, stepID)
	return true
}

func bandwidthProbePreviousStepAckedBytes(state *bandwidthLegState, stepID uint64, step *bandwidthProbeStep) uint64 {
	if state != nil && stepID > 1 {
		if prev := state.steps[stepID-1]; prev != nil && !prev.endedAt.IsZero() {
			return prev.ackedBytes
		}
	}
	if step == nil {
		return 0
	}
	return step.prevAckedBytes
}

func bandwidthProbeStepBps(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	if step.firstRXMS != 0 && step.lastRXMS > step.firstRXMS {
		elapsed := time.Duration(step.lastRXMS-step.firstRXMS) * time.Millisecond
		return step.ackedBytes * 8 * uint64(time.Second) / uint64(elapsed)
	}
	return bandwidthWindowSampleBps(step.ackedBytes, step.startedAt, step.endedAt)
}

func bandwidthProbeStepCeilingBps(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	return bandwidthWindowSampleBps(step.ackedBytes, step.startedAt, step.endedAt)
}

func bandwidthProbeStepSentBps(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	return bandwidthWindowSampleBps(step.sentBytes, step.startedAt, step.endedAt)
}

func updateBandwidthProbeStepRXSpan(step *bandwidthProbeStep, round *bandwidthProbeRound) {
	if step == nil || round == nil || round.firstRXMS == 0 {
		return
	}
	if step.firstRXMS == 0 || round.firstRXMS < step.firstRXMS {
		step.firstRXMS = round.firstRXMS
	}
	if round.lastRXMS > step.lastRXMS {
		step.lastRXMS = round.lastRXMS
	}
}

func bandwidthProbeGrowthStalled(prevBytes, currentBytes uint64) bool {
	if prevBytes == 0 {
		return false
	}
	return currentBytes*bandwidthProbeGrowthMinDen < prevBytes*bandwidthProbeGrowthMinNum
}

func bandwidthProbeUnderDelivered(sampleBps, targetBps uint64) bool {
	if sampleBps == 0 || targetBps == 0 {
		return false
	}
	return sampleBps*bandwidthProbeDeliveryMinDen < targetBps*bandwidthProbeDeliveryMinNum
}

func bandwidthProbeTargetAttempted(sentBps, targetBps uint64) bool {
	if sentBps == 0 || targetBps == 0 {
		return false
	}
	return sentBps*bandwidthProbeAttemptMinDen >= targetBps*bandwidthProbeAttemptMinNum
}

func bandwidthProbeCeilingTargetMature(targetBps uint64) bool {
	return targetBps >= bandwidthProbeCeilingMinTargetBps
}

func bandwidthProbeLossIncreased(prevLoss, currentLoss float64) bool {
	return currentLoss > prevLoss+bandwidthProbeLossIncreaseEpsilon
}

func bandwidthProbeStepAckComplete(step *bandwidthProbeStep) bool {
	sent := stepSentFrames(step)
	if sent == 0 {
		return false
	}
	acked := stepAckedFrames(step)
	return acked*bandwidthProbeStepAckMinDen >= sent*bandwidthProbeStepAckMinNum
}

func stepSentFrames(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	return step.sentFrames
}

func stepAckedFrames(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	return step.ackedFrames
}

func (l *Send) markBandwidthProbeSendComplete(legKey pingKey, endedAt time.Time) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && state.inFlight && state.endedAt.IsZero() {
		state.endedAt = endedAt
	}
	l.bandwidthMu.Unlock()
}

func (l *Send) completeBandwidthProbeTrain(key laneKey, leg transport.LegRef, legKey pingKey) bool {
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		l.bandwidthMu.Unlock()
		return false
	}
	now := time.Now()
	if state.startedAt.IsZero() {
		state.startedAt = now.Add(-bandwidthProbeWindow)
	}
	if state.endedAt.IsZero() {
		state.endedAt = now
	}
	sampleBps := bandwidthWindowSampleBps(state.ackedBytes, state.startedAt, state.endedAt)
	state.ewmaBps = sampleBps
	state.lastLoss = bandwidthAggregateLoss(state.sentFrames, state.ackedFrames)
	state.sampleCount = 1
	state.complete = true
	state.inFlight = false
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
		}
	}
	ewmaBps := state.ewmaBps
	nextRateBps := state.nextRateBps
	sampleCount := state.sampleCount
	aggregateLoss := state.lastLoss
	l.bandwidthMu.Unlock()

	if lane := l.getLane(key); lane != nil {
		lane.recordBandwidthSample(leg.Kind, ewmaBps, aggregateLoss)
	}
	metrics.SetGauge(metrics.LaneBandwidthBps, float64(ewmaBps),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	metrics.SetGauge(metrics.LaneProbeLossRatio, aggregateLoss,
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	metrics.IncCounter(metrics.BandwidthProbeEventsTotal,
		metrics.L("event", "finish"),
		metrics.L("session", key.sessionID),
		metrics.L("lane", key.laneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	l.logBandwidthProbeDecisionIfReady(key)
	debuglog.Printf("send/bw_probe", "train_finish session=%d lane=%d leg={%s} window_loss=%.3f window_bps=%d next_rate_bps=%d samples=%d", key.sessionID, key.laneID, debugLeg(leg), aggregateLoss, ewmaBps, nextRateBps, sampleCount)
	return true
}

func (l *Send) abortBandwidthProbeTrain(legKey pingKey) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && !state.complete {
		state.inFlight = false
	}
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
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
	useUDP, ok := l.legSelector(key.sessionID).Pick(udpQ, tcpQ)
	logBandwidthProbeDecision(key.sessionID, key.laneID, udpQ, tcpQ, useUDP, ok)
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
