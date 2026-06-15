package bw

import (
	"context"
	"errors"
	"math/bits"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

var (
	ErrInvalidProbe = errors.New("bw: invalid probe")
	ErrInvalidAck   = errors.New("bw: invalid ack")
)

// Probe is the semantic bandwidth probe value sent in one probe packet. It does
// not reference protocol frames or transport identifiers (零身份).
type Probe struct {
	ID        uint64 // unique probe train ID
	Seq       uint16 // sequence number within train
	Count     uint16 // total packets in train
	SendMS    uint64 // send timestamp
	Total     uint64 // total bytes in train
	Remaining uint64 // bytes remaining after this packet
	Bytes     int    // bytes in this probe packet
}

// Ack is the semantic acknowledgment value sent after receiving probe packets.
type Ack struct {
	ID        uint64 // probe train ID being acknowledged
	Count     uint16 // total packets in train
	Received  uint64 // bitmap of received packets
	FirstRXMS uint64 // timestamp of first received packet
	LastRXMS  uint64 // timestamp of last received packet
}

// Sample is the calculated bandwidth and loss measurement from a probe train.
type Sample struct {
	BandwidthBps uint64  // measured bandwidth in bits per second (best step)
	Loss         float64 // aggregate loss ratio [0.0, 1.0]
	ReferenceBps uint64  // reference/target bandwidth
}

// Config holds the fixed parameters for a BW instance. Rate adaptation inputs
// (ReferenceBps/CapBps) are injected here; the bw package never reads lane,
// transport, or observer state — it only computes (零身份, spec 5.8 decision).
type Config struct {
	ReferenceBps uint64            // reference bandwidth for probing (UDP cap from TCP reference)
	CapBps       uint64            // hard cap; when >0 the train never exceeds it
	SendProbe    func(Probe) error // callback to send a probe packet
	SendAck      func(Ack) error   // callback to send an ack (passive side; optional)
	OnSample     func(Sample)      // callback when the train's final sample is ready

	// Rate adaptation tuning. Zero values fall back to package defaults.
	MinRateBps uint64        // starting/floor rate
	StepWindow time.Duration // per-step send window
	AckGrace   time.Duration // how long to wait for a step's acks before scoring it

	// Payload sizing (spec: UDP randomizes 1200-1400; TCP fixes 32KB). The bw
	// package stays zero-identity: it only sees these raw byte bounds, not the
	// transport kind. When PayloadMin==PayloadMax the size is fixed; when
	// PayloadMin<PayloadMax each probe picks a size in [min,max]. Zero → minProbeSize.
	PayloadMin int
	PayloadMax int

	// LossStopThreshold is retained for old callers but no longer stops a train;
	// trains either measure near cap or run until TrainWindow.
	LossStopThreshold float64

	// RateLimit enables a token-bucket limiter that paces probe sends to the
	// current step rate (spec: avoid bursting the link). Off by default.
	RateLimit bool
}

// Rate adaptation defaults (mirrors the old bandwidth_probe.go constants,
// trimmed to what the zero-identity loop needs).
const (
	defaultMinRateBps = uint64(16_000_000)
	defaultStepWindow = 200 * time.Millisecond
	defaultAckGrace   = 100 * time.Millisecond
	additiveStepBps   = uint64(10_000_000)
	plateauGrowth     = 1.05
	plateauSteps      = 2
	minProbeSize      = 1200
	maxProbeFrames    = 64
	minProbeFrames    = 2
	capReachedNum     = uint64(995)
	capReachedDen     = uint64(1000)
)

var trainWindow = 2 * time.Second

// nextRate returns the next probing rate given the current rate, an optional
// hard cap, and whether growth has stalled (plateau). Pure function (spec 5.8:
// 纯计算). Mirrors the old nextBandwidthProbeRate.
func nextRate(rate, cap uint64, growthStalled bool) uint64 {
	if cap > 0 && rate >= cap {
		return cap
	}
	if cap == 0 {
		if growthStalled {
			return rate
		}
		return rate + additiveStepBps
	}
	gap := cap - rate
	if gap > rate {
		return rate * 2
	}
	if gap > rate/4 {
		step := gap / 2
		if step < additiveStepBps {
			step = additiveStepBps
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

// plateau reports whether the recent step bandwidths have stopped growing
// meaningfully (within plateauGrowth over the last plateauSteps). Pure function
// (spec 5.8). Mirrors the old bandwidthProbePlateau.
func plateau(stepBps []uint64) bool {
	if len(stepBps) < plateauSteps+1 {
		return false
	}
	n := len(stepBps)
	lastMax := stepBps[n-1]
	if stepBps[n-2] > lastMax {
		lastMax = stepBps[n-2]
	}
	prev := stepBps[n-1-plateauSteps]
	return float64(lastMax) < float64(prev)*plateauGrowth
}

// startRate returns the initial probing rate for the given cap. Pure function.
func startRate(capBps, minRateBps uint64) uint64 {
	if capBps == 0 {
		return minRateBps
	}
	start := capBps / 4
	if start < minRateBps {
		start = minRateBps
	}
	if start > capBps {
		start = capBps
	}
	return start
}

// effectiveRate clamps rate to [minRate, cap]. Pure function.
func effectiveRate(rateBps, capBps, minRateBps uint64) uint64 {
	if rateBps == 0 {
		rateBps = minRateBps
	}
	if capBps > 0 && rateBps > capBps {
		return capBps
	}
	return rateBps
}

// trainBudgetBytes returns the total byte budget for a probe train at the given
// rate over trainWindow. Pure function.
func trainBudgetBytes(rateBps uint64) uint64 {
	return trainBudgetBytesFor(rateBps, trainWindow)
}

func trainBudgetBytesFor(rateBps uint64, window time.Duration) uint64 {
	if rateBps == 0 {
		rateBps = defaultMinRateBps
	}
	if window <= 0 {
		window = trainWindow
	}
	return rateBps * uint64(window) / uint64(time.Second) / 8
}

func trainBudgetRate(referenceBps, capBps, minRateBps uint64) uint64 {
	if referenceBps > 0 {
		return referenceBps
	}
	if capBps > 0 {
		return capBps
	}
	return minRateBps
}

// stepFrameCount derives how many probe frames a step sends at rateBps over the
// step window, clamped to [minProbeFrames, maxProbeFrames]. Pure function.
func stepFrameCount(rateBps uint64, stepWindow time.Duration, bytesPerProbe int) uint16 {
	if bytesPerProbe <= 0 {
		bytesPerProbe = minProbeSize
	}
	bytesPerStep := rateBps * uint64(stepWindow) / uint64(time.Second) / 8
	count := bytesPerStep / uint64(bytesPerProbe)
	if count < minProbeFrames {
		count = minProbeFrames
	}
	if count > maxProbeFrames {
		count = maxProbeFrames
	}
	return uint16(count)
}

// bandwidthBps computes bits-per-second from received bytes over a span. Pure.
func bandwidthBps(receivedBytes, firstRXMS, lastRXMS uint64) uint64 {
	if receivedBytes == 0 || lastRXMS <= firstRXMS {
		return 0
	}
	durationMS := lastRXMS - firstRXMS
	if durationMS == 0 {
		durationMS = 1
	}
	return (receivedBytes * 8 * 1000) / durationMS
}

// BW is the active-side factory: it creates probe trains (BwLoop). It does not
// import send or protocol packages and never encodes frames or writes transport
// packets — all I/O goes through the injected callbacks (零身份).
type BW struct {
	referenceBps uint64
	capBps       uint64
	minRateBps   uint64
	stepWindow   time.Duration
	ackGrace     time.Duration
	payloadMin   int
	payloadMax   int
	rateLimit    bool
	sendProbe    func(Probe) error
	onSample     func(Sample)

	mu          sync.Mutex
	nextTrainID uint64
	activeLoops map[uint64]*BwLoop
}

// New creates a BW active-side factory with the given configuration.
func New(cfg Config) *BW {
	minRate := cfg.MinRateBps
	if minRate == 0 {
		minRate = defaultMinRateBps
	}
	stepWindow := cfg.StepWindow
	if stepWindow <= 0 {
		stepWindow = defaultStepWindow
	}
	ackGrace := cfg.AckGrace
	if ackGrace <= 0 {
		ackGrace = defaultAckGrace
	}
	payloadMin := cfg.PayloadMin
	if payloadMin <= 0 {
		payloadMin = minProbeSize
	}
	payloadMax := cfg.PayloadMax
	if payloadMax < payloadMin {
		payloadMax = payloadMin
	}
	return &BW{
		referenceBps: cfg.ReferenceBps,
		capBps:       cfg.CapBps,
		minRateBps:   minRate,
		stepWindow:   stepWindow,
		ackGrace:     ackGrace,
		payloadMin:   payloadMin,
		payloadMax:   payloadMax,
		rateLimit:    cfg.RateLimit,
		sendProbe:    cfg.SendProbe,
		onSample:     cfg.OnSample,
		activeLoops:  make(map[uint64]*BwLoop),
	}
}

// payloadSize returns the probe payload size for (trainID, seq). When the bounds
// are equal it is fixed; otherwise it is a deterministic pseudo-random size in
// [min,max] seeded by the train/seq so each probe varies without bw knowing the
// transport kind (零身份). Pure function.
func payloadSize(min, max int, trainID uint64, seq uint16) int {
	if max <= min {
		return min
	}
	span := uint64(max - min + 1)
	// Cheap deterministic mix (splitmix64-style) of trainID and seq.
	x := trainID*0x9E3779B97F4A7C15 + uint64(seq)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	return min + int(x%span)
}

// Start begins a new active probe train and returns its BwLoop. The loop runs
// rate-adaptive steps in a background goroutine; inbound acks are fed via
// BwLoop.Ack. When the train finishes (plateau/cap/budget/ctx) it emits the
// final Sample through OnSample and removes itself from the active set.
func (b *BW) Start(ctx context.Context) (*BwLoop, error) {
	if b.sendProbe == nil {
		return nil, errors.New("bw: no send probe callback configured")
	}

	b.mu.Lock()
	trainID := b.nextTrainID
	b.nextTrainID++
	b.mu.Unlock()

	loop := &BwLoop{
		bw:            b,
		trainID:       trainID,
		capBps:        b.capBps,
		referenceBps:  b.referenceBps,
		minRateBps:    b.minRateBps,
		stepWindow:    b.stepWindow,
		ackGrace:      b.ackGrace,
		payloadMin:    b.payloadMin,
		payloadMax:    b.payloadMax,
		bytesPerProbe: (b.payloadMin + b.payloadMax) / 2, // average, for frame-count sizing
	}
	if b.rateLimit {
		// Limiter is sized per step in sendStep; start with the floor rate.
		loop.limiter = rate.NewLimiter(rate.Limit(b.minRateBps/8), b.payloadMax)
	}

	b.mu.Lock()
	b.activeLoops[trainID] = loop
	b.mu.Unlock()

	go loop.run(ctx)
	return loop, nil
}

// BwLoop is one active, rate-adaptive probe train. It sends successive steps at
// increasing rates until the bandwidth plateaus, the cap is reached, or the byte
// budget is spent, then emits a single Sample (best step bandwidth + aggregate
// loss). Inbound acks are fed asynchronously via Ack.
type BwLoop struct {
	bw           *BW
	trainID      uint64
	capBps       uint64
	referenceBps uint64
	minRateBps   uint64
	stepWindow   time.Duration
	ackGrace     time.Duration
	payloadMin   int
	payloadMax   int
	limiter      *rate.Limiter // nil when rate limiting is off

	bytesPerProbe int

	mu          sync.Mutex
	count       uint16 // frames in the current step (last step's count for tests)
	curStep     *stepState
	stepBps     []uint64
	sentFrames  uint64
	ackedFrames uint64
	done        bool
}

// stepState tracks one step's send/ack accounting.
type stepState struct {
	rateBps   uint64
	count     uint16
	sent      uint16
	received  uint64
	firstRXMS uint64
	lastRXMS  uint64
	bytesEach int
	ackCh     chan struct{}
	ackClosed bool
}

// TrainID returns the probe train ID (used as the LaneManager key).
func (l *BwLoop) TrainID() uint64 {
	return l.trainID
}

// Count returns the frame count of the most recent step (test/diagnostic use).
func (l *BwLoop) Count() uint16 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// run drives the rate-adaptive step loop until plateau/cap/budget/ctx, then
// emits the final Sample.
func (l *BwLoop) run(ctx context.Context) {
	defer l.cleanup()

	curRate := startRate(l.capBps, l.minRateBps)
	window := trainWindow
	trainTotal := trainBudgetBytesFor(trainBudgetRate(l.referenceBps, l.capBps, l.minRateBps), window)
	remaining := trainTotal
	deadline := time.Now().Add(window)
	bestBps := uint64(0)

	for remaining > 0 && time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		effRate := effectiveRate(curRate, l.capBps, l.minRateBps)
		count := stepFrameCount(effRate, l.stepWindow, l.bytesPerProbe)

		step := l.beginStep(effRate, count)
		l.sendStep(ctx, step, trainTotal, &remaining)

		// Wait for this step's acks (or grace timeout) before scoring it.
		l.waitStep(ctx, step)
		stepBps := l.scoreStep(step)
		l.mu.Lock()
		l.stepBps = append(l.stepBps, stepBps)
		stalled := plateau(l.stepBps)
		l.mu.Unlock()
		if stepBps > bestBps {
			bestBps = stepBps
		}

		if capReached(bestBps, l.capBps) {
			break
		}
		curRate = nextRate(effRate, l.capBps, stalled)
	}

	if remaining > 0 {
		l.sendComplete(ctx, trainTotal)
	}
	l.emitSample()
}

func capReached(measuredBps, capBps uint64) bool {
	if capBps == 0 || measuredBps == 0 {
		return false
	}
	return measuredBps >= capBps*capReachedNum/capReachedDen
}

func (l *BwLoop) sendComplete(ctx context.Context, trainTotal uint64) {
	if ctx.Err() != nil || l.isDone() || l.bw == nil || l.bw.sendProbe == nil {
		return
	}
	_ = l.bw.sendProbe(Probe{
		ID:        l.trainID,
		Seq:       0,
		Count:     1,
		SendMS:    uint64(time.Now().UnixMilli()),
		Total:     trainTotal,
		Remaining: 0,
		Bytes:     0,
	})
}

func (l *BwLoop) isDone() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done
}

// stepLoss returns the loss ratio of a step from its sent vs acked frame counts.
func (l *BwLoop) stepLoss(step *stepState) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	acked := uint64(bits.OnesCount64(step.received))
	return aggregateLoss(uint64(step.sent), acked)
}

// beginStep installs a fresh step as the current one.
func (l *BwLoop) beginStep(rateBps uint64, count uint16) *stepState {
	step := &stepState{
		rateBps:   rateBps,
		count:     count,
		bytesEach: l.bytesPerProbe,
		ackCh:     make(chan struct{}),
	}
	l.mu.Lock()
	l.curStep = step
	l.count = count
	l.mu.Unlock()
	return step
}

// sendStep emits the step's probe frames, spread across the step window, and
// returns the number of bytes sent. Each frame carries decreasing Remaining so
// the peer can detect train end (Remaining==0 on the final frame). Per-probe
// payload sizes are randomized in [payloadMin,payloadMax] (spec: UDP 1200-1400);
// when a limiter is set, sends are token-bucket paced to the step rate.
func (l *BwLoop) sendStep(ctx context.Context, step *stepState, trainTotal uint64, remaining *uint64) uint64 {
	if remaining == nil || *remaining == 0 {
		return 0
	}
	sizes := make([]int, 0, step.count)
	stepRemaining := *remaining
	for seq := uint16(0); seq < step.count && stepRemaining > 0; seq++ {
		bytes := payloadSize(l.payloadMin, l.payloadMax, l.trainID, seq)
		if uint64(bytes) > stepRemaining {
			bytes = int(stepRemaining)
		}
		sizes = append(sizes, bytes)
		stepRemaining -= uint64(bytes)
	}
	l.mu.Lock()
	step.count = uint16(len(sizes))
	l.count = step.count
	l.mu.Unlock()
	if step.count == 0 {
		return 0
	}

	// Pace the step rate via the limiter (re-sized to this step's rate) or, when
	// rate limiting is off, spread sends evenly across the step window via a ticker.
	if l.limiter != nil {
		now := time.Now()
		l.limiter.SetLimitAt(now, rate.Limit(step.rateBps/8)) // bytes per second
		l.limiter.SetBurstAt(now, l.payloadMax)
	}
	var ticker *time.Ticker
	if l.limiter == nil {
		interval := l.stepWindow / time.Duration(step.count)
		if interval <= 0 {
			interval = time.Millisecond
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
	}

	var sentBytes uint64

	for seq, bytes := range sizes {
		if *remaining == 0 {
			return sentBytes
		}

		// Pace with the limiter when enabled (token-bucket on bytes).
		if l.limiter != nil {
			if err := l.limiter.WaitN(ctx, bytes); err != nil {
				return sentBytes
			}
		}

		nowMS := uint64(time.Now().UnixMilli())
		*remaining -= uint64(bytes)
		probe := Probe{
			ID:        l.trainID,
			Seq:       uint16(seq),
			Count:     step.count,
			SendMS:    nowMS,
			Total:     trainTotal,
			Remaining: *remaining,
			Bytes:     bytes,
		}

		l.mu.Lock()
		step.sent++
		l.sentFrames++
		l.mu.Unlock()

		if err := l.bw.sendProbe(probe); err != nil {
			return sentBytes
		}
		sentBytes += uint64(bytes)

		if l.limiter != nil {
			// The limiter already paced this send; no extra wait.
			select {
			case <-ctx.Done():
				return sentBytes
			default:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return sentBytes
		case <-ticker.C:
		}
	}
	return sentBytes
}

// waitStep blocks until the current step's acks complete or the grace period
// elapses.
func (l *BwLoop) waitStep(ctx context.Context, step *stepState) {
	timer := time.NewTimer(l.ackGrace)
	defer timer.Stop()
	select {
	case <-step.ackCh:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// scoreStep computes the bandwidth observed during a step and folds its acked
// frame count into the loop's aggregate accounting.
func (l *BwLoop) scoreStep(step *stepState) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	acked := bits.OnesCount64(step.received)
	l.ackedFrames += uint64(acked)
	receivedBytes := uint64(acked) * uint64(step.bytesEach)
	return bandwidthBps(receivedBytes, step.firstRXMS, step.lastRXMS)
}

// Ack folds an inbound acknowledgment into the current step. When the step's
// received set is complete it signals the run loop to score and advance.
func (l *BwLoop) Ack(ack Ack) {
	if ack.ID != l.trainID {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	step := l.curStep
	if step == nil || l.done || ack.Count != step.count {
		return
	}
	step.received |= ack.Received
	if ack.FirstRXMS != 0 && (step.firstRXMS == 0 || ack.FirstRXMS < step.firstRXMS) {
		step.firstRXMS = ack.FirstRXMS
	}
	if ack.LastRXMS > step.lastRXMS {
		step.lastRXMS = ack.LastRXMS
	}

	var full uint64
	if step.count >= 64 {
		full = ^uint64(0)
	} else {
		full = (uint64(1) << step.count) - 1
	}
	if step.received&full == full && !step.ackClosed {
		step.ackClosed = true
		close(step.ackCh)
	}
}

// scoreStep results are summarized into the final Sample here.
func (l *BwLoop) emitSample() {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	l.done = true
	var bestBps uint64
	for _, bps := range l.stepBps {
		if bps > bestBps {
			bestBps = bps
		}
	}
	loss := aggregateLoss(l.sentFrames, l.ackedFrames)
	ref := l.referenceBps
	l.mu.Unlock()

	if l.bw != nil && l.bw.onSample != nil {
		l.bw.onSample(Sample{
			BandwidthBps: bestBps,
			Loss:         loss,
			ReferenceBps: ref,
		})
	}
}

// Stop aborts the train without emitting a sample (e.g. the leg died). Safe to
// call multiple times. bw failure must not mark the lane down (spec 5.8), so the
// caller simply drops the loop.
func (l *BwLoop) Stop() {
	l.mu.Lock()
	l.done = true
	l.mu.Unlock()
	l.cleanup()
}

func (l *BwLoop) cleanup() {
	if l.bw == nil {
		return
	}
	l.bw.mu.Lock()
	delete(l.bw.activeLoops, l.trainID)
	l.bw.mu.Unlock()
}

// aggregateLoss returns the loss ratio from sent vs acked frame counts. Pure.
func aggregateLoss(sentFrames, ackedFrames uint64) float64 {
	if sentFrames == 0 {
		return 1
	}
	if ackedFrames > sentFrames {
		ackedFrames = sentFrames
	}
	return float64(sentFrames-ackedFrames) / float64(sentFrames)
}

// Receive is the passive side (spec 5.8): it tracks per-train received bitmaps
// and, for each inbound probe, returns the Ack to send and whether to send it.
// It owns no I/O — the recv glue calls Probe and writes the returned Ack frame.
// It does not import send or protocol packages (零身份).
type Receive struct {
	ackEvery uint16

	mu     sync.Mutex
	rounds map[uint64]*passiveRound
}

// ReceiveConfig tunes the passive side.
type ReceiveConfig struct {
	AckEvery uint16 // send an ack every N received frames (default 16)
}

const defaultAckEvery = 16

// NewReceive creates a passive bandwidth receiver.
func NewReceive(cfg ReceiveConfig) *Receive {
	ackEvery := cfg.AckEvery
	if ackEvery == 0 {
		ackEvery = defaultAckEvery
	}
	return &Receive{
		ackEvery: ackEvery,
		rounds:   make(map[uint64]*passiveRound),
	}
}

// Probe records one inbound probe and returns the Ack to send plus whether an
// ack is due now (spec 5.8: Receive.Probe(p) (Ack, bool)). The caller (recv
// glue) writes the returned Ack as a BW_PROBE_ACK frame when the bool is true.
// When the train completes (full bitmap), its round is dropped.
func (r *Receive) Probe(p Probe) (Ack, bool) {
	if p.Count == 0 || p.Count > 64 || p.Seq >= p.Count {
		return Ack{}, false
	}

	nowMS := uint64(time.Now().UnixMilli())

	r.mu.Lock()
	round := r.rounds[p.ID]
	if round == nil || round.count != p.Count {
		round = &passiveRound{count: p.Count, firstRXMS: nowMS}
		r.rounds[p.ID] = round
	}
	round.received |= uint64(1) << p.Seq
	if round.firstRXMS == 0 || nowMS < round.firstRXMS {
		round.firstRXMS = nowMS
	}
	if nowMS > round.lastRXMS {
		round.lastRXMS = nowMS
	}

	var full uint64
	if round.count >= 64 {
		full = ^uint64(0)
	} else {
		full = (uint64(1) << round.count) - 1
	}
	complete := round.received&full == full
	shouldAck := complete || p.Seq+1 == round.count || (p.Seq+1)%r.ackEvery == 0

	ack := Ack{
		ID:        p.ID,
		Count:     round.count,
		Received:  round.received,
		FirstRXMS: round.firstRXMS,
		LastRXMS:  round.lastRXMS,
	}
	if complete {
		delete(r.rounds, p.ID)
	}
	r.mu.Unlock()

	return ack, shouldAck
}

type passiveRound struct {
	count     uint16
	received  uint64
	firstRXMS uint64
	lastRXMS  uint64
}
