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
	bandwidthProbeAckGrace          = 500 * time.Millisecond
	bandwidthProbeWindow            = 10 * time.Second
	bandwidthProbeRoundWindow       = 500 * time.Millisecond
	bandwidthProbeBurstWindow       = 2 * time.Millisecond
	bandwidthProbeUDPMinPayloadSize = 1200
	bandwidthProbeUDPMaxPayloadSize = 1400
	bandwidthProbeTCPPayloadSize    = 32 * 1024
	bandwidthProbeMaxFrames         = 64
	bandwidthProbeAckEvery          = 16
	bandwidthProbeFrameOverhead     = 30
	bandwidthProbeMinRateBps        = uint64(16_000_000)
	bandwidthProbeAdditiveStepBps   = uint64(10_000_000)
	bandwidthProbePlateauGrowth     = 1.05
	bandwidthProbePlateauSteps      = 2
	bandwidthProbeTCPSafetyWindow   = 10 * time.Second
	bandwidthProbeTCPRateBps        = uint64(16_000_000)
)

func bandwidthProbePlateau(stepBps []uint64) bool {
	if len(stepBps) < bandwidthProbePlateauSteps+1 {
		return false
	}
	n := len(stepBps)
	lastMax := stepBps[n-1]
	if stepBps[n-2] > lastMax {
		lastMax = stepBps[n-2]
	}
	prev := stepBps[n-1-bandwidthProbePlateauSteps]
	return float64(lastMax) < float64(prev)*bandwidthProbePlateauGrowth
}

func nextBandwidthProbeRate(rate, cap uint64, growthStalled bool) uint64 {
	if cap > 0 && rate >= cap {
		return cap
	}
	if cap == 0 {
		if growthStalled {
			return rate
		}
		return rate + bandwidthProbeAdditiveStepBps
	}
	gap := cap - rate
	if gap > rate {
		return rate * 2
	}
	if gap > rate/4 {
		step := gap / 2
		if step < bandwidthProbeAdditiveStepBps {
			step = bandwidthProbeAdditiveStepBps
		}
		next := rate + step
		if next > cap {
			return cap
		}
		return next
	}
	next := rate + 5_000_000
	if next > cap {
		return cap
	}
	return next
}

type bandwidthLegState struct {
	key     laneKey
	capBps  uint64
	rateBps uint64

	inFlight  bool
	complete  bool
	startedAt time.Time
	endedAt   time.Time

	sentFrames  uint64
	ackedFrames uint64
	ackedBytes  uint64

	steps     map[uint64]*bandwidthProbeStep
	stepOrder []uint64
	stepBps   []uint64

	lastLoss float64
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
		if !l.isBandwidthProbeServerReady(item.key) {
			continue
		}
		udpLeg, udpQ, tcpLeg, tcpQ := item.lane.legQualities()
		if _, ok := l.bandwidthProbeCandidate(item.key, item.lane, udpLeg, udpQ, tcpLeg, tcpQ); ok {
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
		if leg, ok := l.bandwidthProbeCandidate(item.key, item.lane, udpLeg, udpQ, tcpLeg, tcpQ); ok {
			candidates = append(candidates, candidate{key: item.key, leg: leg})
		}
		break
	}

	for _, item := range candidates {
		l.maybeStartBandwidthProbe(ctx, item.key, item.leg, now)
	}
}

func (l *Send) bandwidthProbeCandidate(key laneKey, lane *laneRuntime, udpLeg transport.LegRef, udpQ LegQuality, tcpLeg transport.LegRef, tcpQ LegQuality) (transport.LegRef, bool) {
	if l.bandwidthProbeNeeded(tcpLeg, tcpQ) {
		return tcpLeg, true
	}
	if !l.bandwidthProbeNeeded(udpLeg, udpQ) {
		return transport.LegRef{}, false
	}
	if l.bandwidthProbeAwaitingTCPReference(key, lane, tcpLeg, tcpQ) {
		return transport.LegRef{}, false
	}
	return udpLeg, true
}

func (l *Send) bandwidthProbeAwaitingTCPReference(key laneKey, lane *laneRuntime, tcpLeg transport.LegRef, tcpQ LegQuality) bool {
	if tcpQ.ProbeSamples >= minBandwidthProbeSamples {
		return false
	}
	if newPingKey(tcpLeg).kind != 0 {
		return true
	}
	if lane == nil || l.streamTransport == nil {
		return false
	}
	return lane.tcpRemoteSnapshot() != ""
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
		state = &bandwidthLegState{key: key}
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
	state.sentFrames = 0
	state.ackedFrames = 0
	state.ackedBytes = 0
	state.steps = make(map[uint64]*bandwidthProbeStep)
	state.stepOrder = nil
	state.stepBps = nil
	state.lastLoss = 0
	state.capBps = l.bandwidthProbeCapBps
	state.rateBps = bandwidthProbeStartRate(l.bandwidthProbeCapBps)
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "train_start session=%d lane=%d leg={%s} cap_bps=%d start_bps=%d window=%s", key.sessionID, key.laneID, debugLeg(leg), state.capBps, state.rateBps, bandwidthProbeWindow)
	go l.runBandwidthProbeTrain(ctx, key, leg, legKey)
}

func bandwidthProbeStartRate(capBps uint64) uint64 {
	if capBps == 0 {
		return bandwidthProbeMinRateBps
	}
	start := capBps / 4
	if start < bandwidthProbeMinRateBps {
		start = bandwidthProbeMinRateBps
	}
	if start > capBps {
		start = capBps
	}
	return start
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
	isUDP := leg.Kind == transport.KindUDP
	window := bandwidthProbeWindow
	if !isUDP {
		window = bandwidthProbeTCPSafetyWindow
	}
	deadline := time.Now().Add(window)
	endedAt := deadline

	var limiterState bandwidthProbeLimiterState
	var stepID uint64

	for time.Now().Before(deadline) {
		now := time.Now()
		stepDeadline := now.Add(bandwidthProbeRoundWindow)
		if stepDeadline.After(deadline) {
			stepDeadline = deadline
		}
		stepID++
		step := l.startBandwidthProbeStep(legKey, stepID, now)
		if step == nil {
			return
		}
		for time.Now().Before(stepDeadline) {
			round := l.startBandwidthProbeRound(key, leg, legKey, stepID, time.Now())
			if round == nil {
				return
			}
			limiter := limiterState.forRound(leg, round)
			sent := l.runBandwidthProbeRound(ctx, round, stepDeadline, limiter)
			if sent == round.count {
				l.recordBandwidthProbeSent(round, sent)
			}
			if sent < round.count {
				if ctx.Err() != nil || time.Now().Before(deadline) && time.Now().Before(stepDeadline) {
					l.abortBandwidthProbeTrain(legKey)
					return
				}
				break
			}
		}
		time.Sleep(bandwidthProbeAckGrace / 10)
		stepBps, stepLoss := l.finishBandwidthProbeStep(legKey, stepID, stepDeadline)
		if stepBps == 0 && stepLoss >= 1 {
			l.abortBandwidthProbeTrain(legKey)
			return
		}

		if isUDP {
			if stepLoss >= bandwidthProbeLossThreshold {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_udp_loss session=%d lane=%d loss=%.3f", key.sessionID, key.laneID, stepLoss)
				break
			}
		} else {
			if bandwidthProbePlateau(l.getStepBpsSlice(legKey)) {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_tcp_plateau session=%d lane=%d", key.sessionID, key.laneID)
				break
			}
		}

		l.advanceBandwidthProbeRate(legKey)
	}

	l.markBandwidthProbeSendComplete(legKey, endedAt)

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

func (l *Send) getStepBpsSlice(legKey pingKey) []uint64 {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		return nil
	}
	return state.stepBps
}

func (l *Send) bandwidthProbeTCPReferenceBps(key laneKey) uint64 {
	lane := l.getLane(key)
	if lane == nil {
		return 0
	}
	_, _, _, tcpQ := lane.legQualities()
	if tcpQ.ProbeSamples < minBandwidthProbeSamples {
		return 0
	}
	return tcpQ.BandwidthBps
}

func bandwidthProbeStepCeilingBps(step *bandwidthProbeStep) uint64 {
	if step == nil {
		return 0
	}
	return bandwidthWindowSampleBps(step.ackedBytes, step.startedAt, step.endedAt)
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

func (l *Send) startBandwidthProbeRound(key laneKey, leg transport.LegRef, legKey pingKey, stepID uint64, now time.Time) *bandwidthProbeRound {
	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil || !state.inFlight || state.complete {
		l.bandwidthMu.Unlock()
		return nil
	}
	rateBps := state.rateBps
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	probeID := l.nextBWProbeID.Add(1)
	payloadBytes := bandwidthProbeUDPPayloadSize(key, probeID, rateBps)
	if leg.Kind == transport.KindTCP {
		payloadBytes = bandwidthProbeTCPPayloadSize
	}
	frameBytes := bandwidthProbeFrameBytes(payloadBytes)
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
				step = &bandwidthProbeStep{startedAt: round.startedAt, rateBps: round.rateBps}
				state.steps[round.stepID] = step
			}
			if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
				step.startedAt = round.startedAt
			}
			updateBandwidthProbeStepRXSpan(step, round)
			step.ackedBytes += ackedBytes
			step.ackedFrames += uint64(acked)
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
		step = &bandwidthProbeStep{startedAt: round.startedAt, rateBps: round.rateBps}
		state.steps[round.stepID] = step
	}
	if step.startedAt.IsZero() || round.startedAt.Before(step.startedAt) {
		step.startedAt = round.startedAt
	}
	step.sentBytes += uint64(sent) * uint64(round.frameBytes)
	step.sentFrames += uint64(sent)
}

func (l *Send) startBandwidthProbeStep(legKey pingKey, stepID uint64, now time.Time) *bandwidthProbeStep {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || !state.inFlight || state.complete {
		return nil
	}
	if state.steps == nil {
		state.steps = make(map[uint64]*bandwidthProbeStep)
	}
	step := &bandwidthProbeStep{
		startedAt: now,
		rateBps:   state.rateBps,
	}
	state.steps[stepID] = step
	state.stepOrder = append(state.stepOrder, stepID)
	return step
}

func (l *Send) finishBandwidthProbeStep(legKey pingKey, stepID uint64, endedAt time.Time) (uint64, float64) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete {
		return 0, 1
	}
	step := state.steps[stepID]
	if step == nil {
		return 0, 1
	}
	step.endedAt = endedAt
	stepBps := bandwidthProbeStepCeilingBps(step)
	stepLoss := bandwidthAggregateLoss(stepSentFrames(step), stepAckedFrames(step))
	state.stepBps = append(state.stepBps, stepBps)
	return stepBps, stepLoss
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

func (l *Send) advanceBandwidthProbeRate(legKey pingKey) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()

	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete {
		return
	}
	if state.rateBps == 0 {
		state.rateBps = bandwidthProbeMinRateBps
	}
	growthStalled := bandwidthProbePlateau(state.stepBps)
	state.rateBps = nextBandwidthProbeRate(state.rateBps, state.capBps, growthStalled)
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
	var bestBps uint64
	for _, stepID := range state.stepOrder {
		if step := state.steps[stepID]; step != nil && !step.endedAt.IsZero() {
			if bps := bandwidthProbeStepCeilingBps(step); bps > bestBps {
				bestBps = bps
			}
		}
	}
	if bestBps == 0 {
		bestBps = bandwidthWindowSampleBps(state.ackedBytes, state.startedAt, state.endedAt)
	}
	aggregateLoss := bandwidthAggregateLoss(state.sentFrames, state.ackedFrames)
	state.lastLoss = aggregateLoss
	state.complete = true
	state.inFlight = false
	for probeID, round := range l.bandwidthPending {
		if round.legKey == legKey {
			delete(l.bandwidthPending, probeID)
		}
	}
	l.bandwidthMu.Unlock()

	if lane := l.getLane(key); lane != nil {
		lane.recordBandwidthSample(leg.Kind, bestBps, aggregateLoss)
	}
	metrics.SetGauge(metrics.LaneBandwidthBps, float64(bestBps),
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
	debuglog.Printf("send/bw_probe", "train_finish session=%d lane=%d leg={%s} loss=%.3f window_bps=%d", key.sessionID, key.laneID, debugLeg(leg), aggregateLoss, bestBps)
	l.sendBandwidthProbeDone(key, leg, bestBps)
	return true
}

func (l *Send) sendBandwidthProbeDone(key laneKey, leg transport.LegRef, bestBps uint64) {
	frame := protocol.Frame{
		Type:      protocol.TypeBandwidthProbeDone,
		SessionID: key.sessionID,
		LaneID:    key.laneID,
		Body: protocol.BandwidthProbeDoneBody{
			ResultBps: bestBps,
		},
	}
	if err := l.writeControlFrameOnLeg(context.TODO(), leg, frame); err != nil {
		debuglog.Printf("send/bw_probe", "done_send_fail session=%d lane=%d err=%v", key.sessionID, key.laneID, err)
	} else {
		debuglog.Printf("send/bw_probe", "done_sent session=%d lane=%d bps=%d", key.sessionID, key.laneID, bestBps)
	}
}

func (l *Send) EnableBandwidthProbeServerReady() {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	if l.bandwidthProbeServerReady == nil {
		l.bandwidthProbeServerReady = make(map[laneKey]bool)
	}
}

func (l *Send) isBandwidthProbeServerReady(key laneKey) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	if l.bandwidthProbeServerReady == nil {
		return true
	}
	return l.bandwidthProbeServerReady[key]
}

func (l *Send) markBandwidthProbeDone(sessionID uint64, laneID uint8) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	if l.bandwidthProbeServerReady == nil {
		l.bandwidthProbeServerReady = make(map[laneKey]bool)
	}
	l.bandwidthProbeServerReady[laneKey{sessionID: sessionID, laneID: laneID}] = true
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
	_, udpQ, _, tcpQ := lane.legQualities()
	if udpQ.ProbeSamples < minBandwidthProbeSamples || tcpQ.ProbeSamples < minBandwidthProbeSamples {
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
