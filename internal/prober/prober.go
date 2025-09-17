package prober

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const (
	NonceSize           = 8
	_defaultTimeout     = 15
	_defaultTimeoutDur  = _defaultTimeout * time.Second
	_minTimeout         = 500 * time.Millisecond
	_baselineBuffer     = 200 * time.Millisecond
	_maxConsecutiveLoss = 5
	_reconnectInterval  = 5 * time.Second
)

type Event int

const (
	Normal Event = iota
	Unstable
	Lost
	Disconnected
)

func (e Event) String() string {
	switch e {
	case Normal:
		return "normal"
	case Unstable:
		return "unstable"
	case Lost:
		return "lost"
	case Disconnected:
		return "disconnected"
	}
	return "unknown"
}

type packetInfo struct {
	startTime time.Time
	isTimeout bool
}

// HoltSmoothing implements Holt's Double Exponential Smoothing for RTT estimation
type HoltSmoothing struct {
	level       float64 // S_t: smoothed level (current estimated RTT)
	trend       float64 // T_t: smoothed trend (rate of change)
	alpha       float64 // smoothing parameter for level (0.1-0.3 typical)
	beta        float64 // smoothing parameter for trend (0.05-0.15 typical)
	initialized bool
}

func NewHoltSmoothing(alpha, beta float64) *HoltSmoothing {
	return &HoltSmoothing{
		alpha: alpha,
		beta:  beta,
	}
}

func (h *HoltSmoothing) Update(observation float64) {
	if !h.initialized {
		h.level = observation
		h.trend = 0
		h.initialized = true
		return
	}

	// Holt's method formulas:
	// S_t = α * X_t + (1-α) * (S_{t-1} + T_{t-1})
	// T_t = β * (S_t - S_{t-1}) + (1-β) * T_{t-1}

	prevLevel := h.level
	h.level = h.alpha*observation + (1-h.alpha)*(h.level+h.trend)
	h.trend = h.beta*(h.level-prevLevel) + (1-h.beta)*h.trend
}

func (h *HoltSmoothing) Predict(steps int) float64 {
	if !h.initialized {
		return 0
	}
	// Forecast: F_{t+k} = S_t + k * T_t
	return h.level + float64(steps)*h.trend
}

func (h *HoltSmoothing) GetCurrent() float64 {
	return h.level
}

func (h *HoltSmoothing) GetTrend() float64 {
	return h.trend
}

func (h *HoltSmoothing) IsInitialized() bool {
	return h.initialized
}

type gcPacket struct {
	nonce   uint64
	elasped time.Duration
}

type Prober struct {
	on       func(Event)
	state    Event
	proberId uuid.UUID

	in             chan *mempool.Buffer
	out            chan *mempool.Buffer
	ctx            context.Context
	rttEstimator   *HoltSmoothing
	addr           string
	minRtt         float64
	lost           float64
	debit          float64
	deadline       *time.Timer
	reschedule     *time.Timer
	currentTimeout time.Duration

	packetMap        map[uint64]*packetInfo
	lastMaxStartTime int64
	consecutiveLoss  int
	reconnectTimer   *time.Timer
	lastSuccessTime  time.Time
}

func New(ctx context.Context, addr string, on func(Event)) *Prober {
	p := &Prober{
		on:           on,
		ctx:          ctx,
		addr:         addr,
		rttEstimator: NewHoltSmoothing(0.2, 0.1), // α=0.2 for level, β=0.1 for trend
		in:           make(chan *mempool.Buffer, 128),
		out:          make(chan *mempool.Buffer, 128),
		minRtt:       math.MaxFloat64,

		deadline:        time.NewTimer(time.Hour), // Initialize with long duration and stop immediately
		reschedule:      time.NewTimer(time.Hour),
		reconnectTimer:  time.NewTimer(time.Hour),
		packetMap:       make(map[uint64]*packetInfo),
		lastSuccessTime: time.Now(),
	}

	// Stop all timers immediately after creation
	p.deadline.Stop()
	p.reschedule.Stop()
	p.reconnectTimer.Stop()

	return p
}

func (p *Prober) In() chan<- *mempool.Buffer {
	return p.in
}

func (p *Prober) Out() <-chan *mempool.Buffer {
	return p.out
}

func (p *Prober) sendProbePacket() {
	// Header:
	// Byte 1: OpCode
	// Byte 2: Reply Epoch
	// Byte 3-19: Prober ID (16B)
	// Byte 20-28: Nonce (8B)
	packet := mempool.GetWithHeader(NonceSize, protocol.HeaderSize+ProbeHeaderSize)
	packet.ReadFrom(rand.Reader)

	// skip epoch id, it's reset to zero by GetWithHeader
	packet.WriteAt(p.proberId[:], protocol.HeaderSize+1)

	nonce := binary.LittleEndian.Uint64(packet.Bytes())

	p.packetMap[nonce] = &packetInfo{startTime: time.Now()}

	p.out <- packet

	if !p.rttEstimator.IsInitialized() {
		p.currentTimeout = _defaultTimeoutDur
		p.deadline = time.NewTimer(p.currentTimeout)
		return
	}

	// Use Holt's method to predict next RTT + safety margin
	// Predict 1 step ahead and add trend-based adjustment
	predictedRtt := p.rttEstimator.Predict(1)
	trend := p.rttEstimator.GetTrend()

	// Add safety margin based on trend direction
	// If trend is positive (RTT increasing), add more buffer
	// If trend is negative (RTT decreasing), add less buffer
	safetyMultiplier := 2.0
	if trend > 0 {
		safetyMultiplier = 2.5 + math.Min(trend/1000, 1.0) // Cap at 3.5x
	} else {
		safetyMultiplier = 1.8 - math.Min(-trend/1000, 0.3) // Floor at 1.5x
	}

	p.currentTimeout = time.Duration(predictedRtt*safetyMultiplier)*time.Microsecond + _baselineBuffer

	if p.deadline.C == nil {
		p.deadline = time.NewTimer(p.currentTimeout)
		return
	}
	p.deadline.Reset(p.currentTimeout)
}

func (p *Prober) markTimeout() (hasTimeouts bool) {
	var timeHeap maxTimeHeap
	var timeoutCount int

	// Check if we should transition to disconnected state
	// Only after being in Lost state for extended period
	if p.state == Lost && time.Since(p.lastSuccessTime) >= _defaultTimeoutDur {
		p.switchState(Disconnected)
		hasTimeouts = true
	}

	// too many on flight packets, need garbage collection
	needGC := len(p.packetMap) > 100

	for nonce := range p.packetMap {
		pkt := p.packetMap[nonce]
		elapsed := time.Since(pkt.startTime)

		// Mark individual packet timeout based on dynamic threshold
		if !pkt.isTimeout && elapsed >= p.currentTimeout {
			pkt.isTimeout = true
			timeoutCount++
		}

		// Hard timeout for very old packets (15s)
		if elapsed >= _defaultTimeoutDur {
			timeoutCount++
		}

		if needGC {
			heap.Push(&timeHeap, &gcPacket{
				nonce:   nonce,
				elasped: elapsed,
			})
			p.lastMaxStartTime = max(p.lastMaxStartTime, pkt.startTime.Unix())
		}
	}

	// Update consecutive loss counter
	if timeoutCount > 0 {
		p.consecutiveLoss++
		hasTimeouts = true

		// State transitions based on consecutive timeouts
		switch p.state {
		case Normal:
			if p.consecutiveLoss >= 2 {
				p.switchState(Unstable)
			}
		case Unstable:
			if p.consecutiveLoss >= _maxConsecutiveLoss {
				p.switchState(Lost)
			}
		case Lost:
			// Already in Lost state, will transition to Disconnected by time check above
		}
	}

	// Garbage collection
	if needGC {
		const exceedSize = 50
		for len(p.packetMap) > exceedSize {
			pkt := heap.Pop(&timeHeap).(*gcPacket)
			delete(p.packetMap, pkt.nonce)
		}
	}

	return hasTimeouts
}

func (p *Prober) recvProbePacket(packet *mempool.Buffer) {
	defer mempool.Put(packet)

	nonce := binary.LittleEndian.Uint64(packet.Bytes())

	info, ok := p.packetMap[nonce]

	if !ok {
		// has been GC or unknown
		return
	}
	defer delete(p.packetMap, nonce)

	elapsedTimeDur := time.Since(info.startTime)

	elapsedTimeUs := elapsedTimeDur.Microseconds()

	// check twice, this aims to avoid the case receiving probe packet and reaching deadline concurrently.
	isTimeout := info.isTimeout ||
		(p.currentTimeout > 0 && elapsedTimeUs >= p.currentTimeout.Microseconds())

	if isTimeout {
		if p.debit > 0 {
			p.debit = 10
		}
		return
	}

	// Successful packet received - reset counters
	p.lastMaxStartTime = 0
	p.consecutiveLoss = 0
	p.lastSuccessTime = time.Now()
	p.deadline.Stop()

	// Handle state recovery
	if p.debit > 0 {
		p.debit--

		if p.debit == 0 {
			switch p.state {
			case Unstable:
				p.switchState(Normal)
			case Lost:
				p.switchState(Normal)
			case Disconnected:
				p.switchState(Lost)
			}
		}
	} else {
		// Immediate recovery for unstable state
		if p.state == Unstable {
			p.switchState(Normal)
		}
	}

	elapsedTime := float64(elapsedTimeUs)

	p.minRtt = min(elapsedTime, p.minRtt)

	// Update Holt smoothing with new RTT measurement
	p.rttEstimator.Update(elapsedTime)

	// Use Holt's method to predict next RTT for scheduling
	predictedRtt := p.rttEstimator.Predict(1)
	trend := p.rttEstimator.GetTrend()

	// Calculate adaptive safety margin based on trend
	safetyMultiplier := 2.0
	if trend > 0 {
		safetyMultiplier = 2.5 + math.Min(trend/1000, 1.0)
	} else {
		safetyMultiplier = 1.8 - math.Min(-trend/1000, 0.3)
	}

	nextTimeout := time.Duration(predictedRtt*safetyMultiplier)*time.Microsecond + _baselineBuffer

	// Schedule next probe with adaptive interval based on state
	var probeInterval time.Duration
	switch p.state {
	case Normal:
		probeInterval = nextTimeout
	case Unstable:
		// Increase probe frequency for unstable connections
		probeInterval = nextTimeout / 2
		if probeInterval < 100*time.Millisecond {
			probeInterval = 100 * time.Millisecond
		}
	case Lost:
		// Reduce probe frequency for lost connections
		probeInterval = nextTimeout * 2
	case Disconnected:
		// Low frequency probing for disconnected state
		probeInterval = _reconnectInterval
	}

	if p.reschedule.C == nil {
		p.reschedule = time.NewTimer(probeInterval)
		return
	}
	p.reschedule.Reset(probeInterval)
}

func (p *Prober) switchState(to Event) {
	if p.state == to {
		return
	}

	// Allow recovery from any state
	oldState := p.state
	p.state = to

	switch to {
	case Normal:
		p.debit = 0
		p.consecutiveLoss = 0
		p.reconnectTimer.Stop()
	case Unstable:
		p.debit = 3 // Require fewer successful packets to recover
		p.reconnectTimer.Stop()
	case Lost:
		p.lost++
		p.debit = 10
		p.reconnectTimer.Stop()
	case Disconnected:
		p.debit = 1
		// Start reconnection timer for periodic probing
		if p.reconnectTimer.C == nil {
			p.reconnectTimer = time.NewTimer(_reconnectInterval)
		} else {
			p.reconnectTimer.Reset(_reconnectInterval)
		}
	}

	fmt.Printf("Switch State %s: %s => %s Debit: %f ConsecutiveLoss: %d RTT: %.2fms Trend: %.2f\n",
		p.addr, oldState, p.state, p.debit, p.consecutiveLoss,
		p.rttEstimator.GetCurrent()/1000, p.rttEstimator.GetTrend())

	p.on(to)
}

// Enhanced State Machine:
// Normal -> Unstable (2 consecutive timeouts)
// Unstable -> Normal (1 successful packet)
// Unstable -> Lost (5 consecutive timeouts)
// Lost -> Normal (10 successful packets)
// Lost -> Disconnected (15s without successful response)
// Disconnected -> Lost (1 successful packet)
func (p *Prober) start() {
	p.sendProbePacket()

	for {
		select {
		case <-p.ctx.Done():
			return
		case pkt := <-p.in:
			p.recvProbePacket(pkt)
		case <-p.reschedule.C:
			p.sendProbePacket()
		case <-p.deadline.C:
			// Check for timeouts and handle state transitions
			if p.markTimeout() {
				// Continue probing even in bad states for potential recovery
				p.sendProbePacket()
			}
		case <-p.reconnectTimer.C:
			// Periodic reconnection attempts for disconnected state
			if p.state == Disconnected {
				p.sendProbePacket()
				p.reconnectTimer.Reset(_reconnectInterval)
			}
		}
	}
}

func (p *Prober) Start(proberId uuid.UUID) {
	p.proberId = proberId
	go p.start()
}
