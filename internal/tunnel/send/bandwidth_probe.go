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
	legpkg "github.com/MeteorsLiu/multipath/internal/tunnel/send/leg"
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
	bandwidthProbeTCPBudgetBps      = uint64(200_000_000)
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

	trainID             uint64
	trainBytesTotal     uint64
	trainBytesRemaining uint64

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
	key                 laneKey
	leg                 transport.LegRef
	legKey              pingKey
	stepID              uint64
	probeID             uint64
	count               uint16
	payloadBytes        int
	frameBytes          int
	rateBps             uint64
	trainID             uint64
	trainBytesTotal     uint64
	trainBytesRemaining uint64
	startedAt           time.Time
	received            uint64
	firstRXMS           uint64
	lastRXMS            uint64
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

type bandwidthRemoteTrainKey struct {
	legKey  pingKey
	trainID uint64
}

type bandwidthRemoteTrain struct {
	key        laneKey
	leg        transport.LegRef
	trainID    uint64
	totalBytes uint64
	firstRXMS  uint64
	lastRXMS   uint64
	timer      *time.Timer
}

type bandwidthProbePhase uint8

const (
	bandwidthProbePhaseLocal bandwidthProbePhase = iota
	bandwidthProbePhaseRemote
)

type bandwidthProbeGate struct {
	sessionID  uint64
	laneID     uint8
	legKind    transport.Kind
	phase      bandwidthProbePhase
	localFirst bool
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

	if _, ok := l.activeBandwidthLane(sessionID); ok {
		return
	}
	var selectedLane laneKey
	for _, item := range lanes {
		udpLeg, udpQ, tcpLeg, tcpQ := item.lane.legQualities()
		if leg, ok := l.bandwidthProbeCandidate(item.key, item.lane, udpLeg, udpQ, tcpLeg, tcpQ); ok &&
			l.bandwidthProbeGateAllowsLocal(item.key, leg.Kind) {
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

func (l *Send) bandwidthProbeCandidate(key laneKey, lane *laneRuntime, udpLeg transport.LegRef, udpQ legpkg.Quality, tcpLeg transport.LegRef, tcpQ legpkg.Quality) (transport.LegRef, bool) {
	if l.bandwidthProbeCapBps > 0 {
		if l.bandwidthProbeNeeded(udpLeg, udpQ) {
			return udpLeg, true
		}
		return transport.LegRef{}, false
	}
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

func (l *Send) bandwidthProbeAwaitingTCPReference(key laneKey, lane *laneRuntime, tcpLeg transport.LegRef, tcpQ legpkg.Quality) bool {
	if tcpQ.ProbeSamples >= legpkg.MinBandwidthProbeSamples {
		return false
	}
	if !tcpQ.Active {
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

func (l *Send) bandwidthProbeNeeded(ref transport.LegRef, quality legpkg.Quality) bool {
	if !quality.Active && !bandwidthProbeCanUseInactiveLeg(ref) {
		return false
	}
	legKey := newPingKey(ref)
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

func (l *Send) setBandwidthProbeGateMode(localFirst bool) {
	sessionID, ok := l.activeSession()
	if !ok {
		return
	}
	l.bandwidthMu.Lock()
	gate := l.bandwidthProbeGateLocked(sessionID)
	gate.localFirst = localFirst
	if localFirst {
		gate.phase = bandwidthProbePhaseLocal
	} else {
		gate.phase = bandwidthProbePhaseRemote
	}
	l.bandwidthGates[sessionID] = gate
	l.bandwidthMu.Unlock()
}

func (l *Send) bandwidthProbeGateLocked(sessionID uint64) bandwidthProbeGate {
	if gate, ok := l.bandwidthGates[sessionID]; ok {
		return gate
	}
	return bandwidthProbeGate{
		sessionID:  sessionID,
		laneID:     1,
		legKind:    l.firstBandwidthProbeLegKind(),
		phase:      bandwidthProbePhaseLocal,
		localFirst: true,
	}
}

func (l *Send) firstBandwidthProbeLegKind() transport.Kind {
	if l.bandwidthProbeCapBps > 0 {
		return transport.KindUDP
	}
	return transport.KindTCP
}

func (l *Send) bandwidthProbeGateAllowsLocal(key laneKey, kind transport.Kind) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	gate := l.bandwidthProbeGateLocked(key.sessionID)
	return gate.phase == bandwidthProbePhaseLocal &&
		gate.sessionID == key.sessionID &&
		gate.laneID == key.laneID &&
		gate.legKind == kind
}

func (l *Send) bandwidthProbeGateAllowsRemote(key laneKey, kind transport.Kind) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	gate := l.bandwidthProbeGateLocked(key.sessionID)
	return gate.phase == bandwidthProbePhaseRemote &&
		gate.sessionID == key.sessionID &&
		gate.laneID == key.laneID &&
		gate.legKind == kind
}

func (l *Send) advanceBandwidthProbeGateAfterLocal(key laneKey, kind transport.Kind) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	gate := l.bandwidthProbeGateLocked(key.sessionID)
	if gate.sessionID != key.sessionID || gate.laneID != key.laneID || gate.legKind != kind ||
		gate.phase != bandwidthProbePhaseLocal {
		return
	}
	if gate.localFirst {
		gate.phase = bandwidthProbePhaseRemote
	} else {
		gate = l.nextBandwidthProbeGateAfterPair(gate)
		gate.phase = bandwidthProbePhaseRemote
	}
	l.bandwidthGates[key.sessionID] = gate
}

func (l *Send) advanceBandwidthProbeGateAfterRemote(key laneKey, kind transport.Kind) {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	gate := l.bandwidthProbeGateLocked(key.sessionID)
	if gate.sessionID != key.sessionID || gate.laneID != key.laneID || gate.legKind != kind ||
		gate.phase != bandwidthProbePhaseRemote {
		return
	}
	if gate.localFirst {
		gate = l.nextBandwidthProbeGateAfterPair(gate)
		gate.phase = bandwidthProbePhaseLocal
	} else {
		gate.phase = bandwidthProbePhaseLocal
	}
	l.bandwidthGates[key.sessionID] = gate
}

func (l *Send) nextBandwidthProbeGateAfterPair(gate bandwidthProbeGate) bandwidthProbeGate {
	if l.bandwidthProbeCapBps == 0 && gate.legKind == transport.KindTCP {
		gate.legKind = transport.KindUDP
		return gate
	}
	gate.laneID++
	gate.legKind = l.firstBandwidthProbeLegKind()
	return gate
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
	capBps := l.bandwidthProbeCapBps
	if leg.Kind == transport.KindUDP && capBps == 0 {
		if ref := l.bandwidthProbeTCPReferenceBps(key); ref > 0 {
			capBps = ref
		}
	}
	state.capBps = capBps
	state.rateBps = bandwidthProbeStartRate(capBps)
	state.trainID = l.nextBWProbeID.Add(1)
	if leg.Kind == transport.KindTCP && capBps == 0 {
		state.trainBytesTotal = bandwidthProbeTCPReferenceTrainBudgetBytes()
	} else {
		state.trainBytesTotal = bandwidthProbeTrainBudgetBytes(capBps)
	}
	state.trainBytesRemaining = state.trainBytesTotal
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

func bandwidthProbeEffectiveRate(rateBps, capBps uint64) uint64 {
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	if capBps > 0 && rateBps > capBps {
		return capBps
	}
	return rateBps
}

func bandwidthProbeTrainBudgetBytes(rateBps uint64) uint64 {
	if rateBps == 0 {
		rateBps = bandwidthProbeMinRateBps
	}
	return rateBps * uint64(bandwidthProbeWindow) / uint64(time.Second) / 8
}

func bandwidthProbeTCPReferenceTrainBudgetBytes() uint64 {
	return bandwidthProbeTrainBudgetBytes(bandwidthProbeTCPBudgetBps)
}

func bandwidthProbeConsumeBudget(remaining uint64, frameBytes int) (uint64, bool) {
	if frameBytes <= 0 {
		return remaining, remaining == 0
	}
	frame := uint64(frameBytes)
	if frame >= remaining {
		return 0, true
	}
	return remaining - frame, false
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
			l.abortBandwidthProbeTrain(legKey)
			return
		}
		for time.Now().Before(stepDeadline) {
			if l.bandwidthProbeTrainBudgetDepleted(legKey) {
				break
			}
			round := l.startBandwidthProbeRound(key, leg, legKey, stepID, time.Now())
			if round == nil {
				l.abortBandwidthProbeTrain(legKey)
				return
			}
			limiter := limiterState.forRound(leg, round)
			sent := l.runBandwidthProbeRound(ctx, round, stepDeadline, limiter)
			if sent > 0 {
				l.recordBandwidthProbeSent(round, sent)
			}
			if sent < round.count {
				if l.bandwidthProbeTrainBudgetDepleted(legKey) {
					break
				}
				if ctx.Err() != nil || time.Now().Before(deadline) && time.Now().Before(stepDeadline) {
					l.abortBandwidthProbeTrain(legKey)
					return
				}
				break
			}
			if l.bandwidthProbeTrainBudgetDepleted(legKey) {
				break
			}
		}
		time.Sleep(bandwidthProbeAckGrace / 10)
		_, stepLoss := l.finishBandwidthProbeStep(legKey, stepID, stepDeadline)

		if isUDP {
			if stepLoss >= legpkg.BandwidthProbeLossThreshold {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_udp_loss session=%d lane=%d loss=%.3f", key.sessionID, key.laneID, stepLoss)
				break
			}
			if l.bandwidthProbeUDPUnderDelivery(legKey) {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_udp_under_delivery session=%d lane=%d", key.sessionID, key.laneID)
				break
			}
		} else {
			if bandwidthProbePlateau(l.getStepBpsSlice(legKey)) {
				endedAt = time.Now()
				debuglog.Printf("send/bw_probe", "stop_tcp_plateau session=%d lane=%d", key.sessionID, key.laneID)
				break
			}
		}

		if l.bandwidthProbeTrainBudgetDepleted(legKey) {
			break
		}
		l.advanceBandwidthProbeRate(legKey)
	}

	l.markBandwidthProbeSendComplete(legKey, endedAt)
	l.advanceBandwidthProbeGateAfterLocal(key, leg.Kind)

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

func (l *Send) bandwidthProbeUDPUnderDelivery(legKey pingKey) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	state := l.bandwidthLegs[legKey]
	if state == nil {
		return false
	}
	return bandwidthProbeUDPUnderDelivery(state.capBps, state.rateBps, state.stepBps)
}

func bandwidthProbeUDPUnderDelivery(capBps, rateBps uint64, stepBps []uint64) bool {
	return capBps > 0 && rateBps < capBps && bandwidthProbePlateau(stepBps)
}

func (l *Send) bandwidthProbeTCPReferenceBps(key laneKey) uint64 {
	lane := l.getLane(key)
	if lane == nil {
		return 0
	}
	_, _, _, tcpQ := lane.legQualities()
	if tcpQ.ProbeSamples < legpkg.MinBandwidthProbeSamples {
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
	rateBps := bandwidthProbeEffectiveRate(state.rateBps, state.capBps)
	probeID := l.nextBWProbeID.Add(1)
	payloadBytes := bandwidthProbeUDPPayloadSize(key, probeID, rateBps)
	if leg.Kind == transport.KindTCP {
		payloadBytes = bandwidthProbeTCPPayloadSize
	}
	frameBytes := bandwidthProbeFrameBytes(payloadBytes)
	round := &bandwidthProbeRound{
		key:                 key,
		leg:                 leg,
		legKey:              legKey,
		stepID:              stepID,
		probeID:             probeID,
		count:               probeFrameCount(rateBps, frameBytes),
		payloadBytes:        payloadBytes,
		frameBytes:          frameBytes,
		rateBps:             rateBps,
		trainID:             state.trainID,
		trainBytesTotal:     state.trainBytesTotal,
		trainBytesRemaining: state.trainBytesRemaining,
		startedAt:           now,
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
		remaining, last := bandwidthProbeConsumeBudget(round.trainBytesRemaining, round.frameBytes)
		round.trainBytesRemaining = remaining
		frame := protocol.Frame{
			Type:      protocol.TypeBandwidthProbe,
			SessionID: round.key.sessionID,
			LaneID:    round.key.laneID,
			Body: protocol.BandwidthProbeBody{
				TrainID:             round.trainID,
				ProbeID:             round.probeID,
				Seq:                 seq,
				Count:               round.count,
				SendMS:              nowMS,
				TrainBytesTotal:     round.trainBytesTotal,
				TrainBytesRemaining: remaining,
				Payload:             payload,
			},
		}
		if err := l.writeControlFrameOnLeg(ctx, round.leg, frame); err != nil {
			debuglog.Printf("send/bw_probe", "send_err session=%d lane=%d leg={%s} probe_id=%d seq=%d err=%v", round.key.sessionID, round.key.laneID, debugLeg(round.leg), round.probeID, seq, err)
			return seq
		}
		l.updateBandwidthProbeTrainRemaining(round.legKey, remaining)
		if last {
			l.advanceBandwidthProbeGateAfterLocal(round.key, round.leg.Kind)
			return seq + 1
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
	if body.Count == 0 || body.Count > 64 || body.Seq >= body.Count ||
		body.TrainBytesTotal == 0 || body.TrainBytesRemaining > body.TrainBytesTotal {
		debuglog.Printf("send/bw_probe", "probe_drop invalid session=%d lane=%d probe_id=%d seq=%d count=%d", sessionID, laneID, body.ProbeID, body.Seq, body.Count)
		return nil
	}
	key := laneKey{sessionID: sessionID, laneID: laneID}
	if !l.bandwidthProbeGateAllowsRemote(key, leg.Kind) {
		debuglog.Printf("send/bw_probe", "probe_drop gate_blocked session=%d lane=%d leg={%s} train_id=%d probe_id=%d", sessionID, laneID, debugLeg(leg), body.TrainID, body.ProbeID)
		return nil
	}

	nowMS := uint64(time.Now().UnixMilli())
	legKey := newPingKey(leg)
	rxKey := bandwidthRXKey{legKey: legKey, probeID: body.ProbeID}
	trainKey := bandwidthRemoteTrainKey{legKey: legKey, trainID: body.TrainID}
	l.bandwidthMu.Lock()
	train := l.bandwidthRemoteTrains[trainKey]
	if train == nil {
		train = &bandwidthRemoteTrain{
			key:        key,
			leg:        leg,
			trainID:    body.TrainID,
			totalBytes: body.TrainBytesTotal,
			firstRXMS:  nowMS,
		}
		l.bandwidthRemoteTrains[trainKey] = train
	} else if train.totalBytes != body.TrainBytesTotal {
		l.bandwidthMu.Unlock()
		debuglog.Printf("send/bw_probe", "probe_drop train_total_changed session=%d lane=%d leg={%s} train_id=%d old_total=%d new_total=%d", sessionID, laneID, debugLeg(leg), body.TrainID, train.totalBytes, body.TrainBytesTotal)
		return nil
	}
	if train.firstRXMS == 0 || nowMS < train.firstRXMS {
		train.firstRXMS = nowMS
	}
	if nowMS > train.lastRXMS {
		train.lastRXMS = nowMS
	}
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
	l.armRemoteBandwidthTrainTimer(key, leg, body.TrainID)

	if body.TrainBytesRemaining == 0 {
		l.completeRemoteBandwidthProbeTrain(key, leg, body.TrainID, "remaining_zero")
	}
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

func (l *Send) remoteBandwidthTrainIdleTimeout(key laneKey, leg transport.LegRef) time.Duration {
	lane := l.getLane(key)
	if lane != nil {
		var srtt time.Duration
		switch leg.Kind {
		case transport.KindUDP:
			srtt = lane.quality.UDP(false).SmoothedRTT
		case transport.KindTCP:
			srtt = lane.quality.TCP(false).SmoothedRTT
		}
		if srtt > 0 {
			srttMS := uint32(srtt / time.Millisecond)
			timeout := time.Duration(srttMS) * time.Millisecond * 8
			if timeout < 500*time.Millisecond {
				return 500 * time.Millisecond
			}
			if timeout > 10*time.Second {
				return 10 * time.Second
			}
			return timeout
		}
	}
	if l.probeTimeout > 0 {
		return l.probeTimeout
	}
	return 10 * time.Second
}

func (l *Send) armRemoteBandwidthTrainTimer(key laneKey, leg transport.LegRef, trainID uint64) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}
	timeout := l.remoteBandwidthTrainIdleTimeout(key, leg)
	trainKey := bandwidthRemoteTrainKey{legKey: legKey, trainID: trainID}
	l.bandwidthMu.Lock()
	train := l.bandwidthRemoteTrains[trainKey]
	if train == nil {
		l.bandwidthMu.Unlock()
		return
	}
	if train.timer != nil {
		train.timer.Stop()
	}
	train.timer = time.AfterFunc(timeout, func() {
		l.completeRemoteBandwidthProbeTrain(key, leg, trainID, "idle_timeout")
	})
	l.bandwidthMu.Unlock()
}

func (l *Send) completeRemoteBandwidthProbeTrain(key laneKey, leg transport.LegRef, trainID uint64, reason string) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}
	trainKey := bandwidthRemoteTrainKey{legKey: legKey, trainID: trainID}
	l.bandwidthMu.Lock()
	if train := l.bandwidthRemoteTrains[trainKey]; train != nil {
		if train.timer != nil {
			train.timer.Stop()
		}
		delete(l.bandwidthRemoteTrains, trainKey)
	}
	for rxKey := range l.bandwidthRX {
		if rxKey.legKey == legKey {
			delete(l.bandwidthRX, rxKey)
		}
	}
	l.bandwidthMu.Unlock()

	debuglog.Printf("send/bw_probe", "remote_train_finish session=%d lane=%d leg={%s} train_id=%d reason=%s", key.sessionID, key.laneID, debugLeg(leg), trainID, reason)
	l.advanceBandwidthProbeGateAfterRemote(key, leg.Kind)
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
	rateBps := bandwidthProbeEffectiveRate(state.rateBps, state.capBps)
	step := &bandwidthProbeStep{
		startedAt: now,
		rateBps:   rateBps,
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

func (l *Send) updateBandwidthProbeTrainRemaining(legKey pingKey, remaining uint64) {
	l.bandwidthMu.Lock()
	if state := l.bandwidthLegs[legKey]; state != nil && state.inFlight && !state.complete {
		state.trainBytesRemaining = remaining
	}
	l.bandwidthMu.Unlock()
}

func (l *Send) bandwidthProbeTrainBudgetDepleted(legKey pingKey) bool {
	l.bandwidthMu.Lock()
	defer l.bandwidthMu.Unlock()
	state := l.bandwidthLegs[legKey]
	return state != nil && state.inFlight && state.trainBytesTotal > 0 && state.trainBytesRemaining == 0
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
	if state == nil || state.complete {
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
		lane.quality.OnBandwidth(leg.Kind, bestBps, aggregateLoss, state.capBps)
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
	return true
}

func (l *Send) completeBandwidthProbeLostLeg(key laneKey, leg transport.LegRef, reason string) {
	legKey := newPingKey(leg)
	if legKey.kind == 0 {
		return
	}

	l.bandwidthMu.Lock()
	state := l.bandwidthLegs[legKey]
	if state == nil || state.complete || !state.inFlight {
		l.bandwidthMu.Unlock()
		return
	}
	now := time.Now()
	if state.startedAt.IsZero() {
		state.startedAt = now
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
		lane.quality.OnBandwidth(leg.Kind, bestBps, aggregateLoss, state.capBps)
	}
	l.logBandwidthProbeDecisionIfReady(key)
	debuglog.Printf("send/bw_probe", "train_finish_lost session=%d lane=%d leg={%s} reason=%s loss=%.3f window_bps=%d", key.sessionID, key.laneID, debugLeg(leg), reason, aggregateLoss, bestBps)
	l.advanceBandwidthProbeGateAfterLocal(key, leg.Kind)
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
	if udpQ.ProbeSamples < legpkg.MinBandwidthProbeSamples {
		return
	}
	if l.bandwidthProbeCapBps == 0 && tcpQ.ProbeSamples < legpkg.MinBandwidthProbeSamples {
		return
	}
	useUDP, ok := l.legSelector(key.sessionID).Pick(udpQ, tcpQ)
	logBandwidthProbeDecision(key.sessionID, key.laneID, udpQ, tcpQ, l.bandwidthProbeCapBps, useUDP, ok)
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
	for key, train := range l.bandwidthRemoteTrains {
		if key.legKey == legKey {
			if train != nil && train.timer != nil {
				train.timer.Stop()
			}
			delete(l.bandwidthRemoteTrains, key)
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
