package ping

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrInvalidPong = errors.New("ping: invalid pong")
	ErrTimeout     = errors.New("ping: timeout")
)

// Message is the semantic ping/pong value. It does not reference protocol
// frames or transport identifiers.
type Message struct {
	ID     uint64
	TimeMS uint64
}

// Quality holds RTT estimation state. All values are in milliseconds.
type Quality struct {
	SampleMS uint32 // latest RTT sample
	SRTTMS   uint32 // smoothed RTT
	RTTVarMS uint32 // RTT variance
	Samples  uint32 // total samples collected
}

// Ping manages active PING timing, pending PING bookkeeping, PONG validation,
// RTT estimation, and liveness (up/down) judgment for one concrete lane
// transport path. It does not import send or protocol packages and never
// encodes frames or writes transport packets. It carries no transport.Kind,
// laneID, or sessionID — identity lives in the owner's closures (spec: 零身份).
//
// The owning lane/transport runtime creates one Ping instance with fixed
// identity, interval, timeout, and callbacks already bound. Start emits
// Message values through sendMsg; Pong validates replies and updates RTT.
//
// Liveness (spec 5.5, 6.2): consecutive ping timeouts accumulate lossCount; on
// reaching MaxLoss the path is declared dead and OnDown fires once. After death,
// consecutive successful pongs accumulate recoverCount; on reaching
// RecoverSuccess the path is declared alive again and OnUp fires once.
type Ping struct {
	interval time.Duration
	timeout  time.Duration
	sendMsg  func(Message) error

	maxLoss        int
	recoverSuccess int
	onDown         func()
	onUp           func()
	observer       func(Quality)
	onDelivery     func(onTime bool)

	mu           sync.Mutex
	nextID       uint64
	pending      map[uint64]uint64 // id -> send time MS
	srtt         uint32
	rttVar       uint32
	sampleCount  uint32
	lastSampleMS uint32

	// Liveness state (spec 5.5).
	lossCount    int
	recoverCount int
	dead         bool
}

// Default liveness thresholds (spec 5.5: MaxLoss 用老的 3) and timing. Zero
// Interval/Timeout in Config fall back to these so Start never builds a ticker
// with a non-positive interval.
const (
	defaultMaxLoss        = 3
	defaultRecoverSuccess = 3
	defaultInterval       = time.Second
	defaultTimeout        = 2 * time.Second
)

// Config holds the fixed parameters for a Ping instance.
type Config struct {
	Interval time.Duration       // ping interval
	Timeout  time.Duration       // ping timeout
	SendMsg  func(Message) error // callback to send ping message

	// Liveness tuning (spec 5.5). Zero values fall back to defaults.
	MaxLoss        int           // consecutive timeouts → dead (default 3)
	RecoverSuccess int           // consecutive pongs after death → alive (default 3)
	OnDown         func()        // fired once when the path is declared dead
	OnUp           func()        // fired once when a dead path recovers
	Observer       func(Quality) // RTT sample sink (adapter feeds leg.observer)

	// OnDelivery reports each ping outcome for delivery-rate tracking (spec 5.4):
	// onTime=true when a pong arrives, false when a ping times out. The adapter
	// feeds leg.observer.OnDelivery so the selector's loss-shaped QoS works.
	OnDelivery func(onTime bool)

	// InitDead starts the path in the dead state so the first RecoverSuccess
	// pongs trigger OnUp (spec: initial activation goes through首批 PONG, not
	// HELLO_ACK). A leg begins inactive; only peer replies bring it up.
	InitDead bool
}

// New creates a Ping instance with the given configuration.
func New(cfg Config) *Ping {
	maxLoss := cfg.MaxLoss
	if maxLoss <= 0 {
		maxLoss = defaultMaxLoss
	}
	recoverSuccess := cfg.RecoverSuccess
	if recoverSuccess <= 0 {
		recoverSuccess = defaultRecoverSuccess
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Ping{
		interval:       interval,
		timeout:        timeout,
		sendMsg:        cfg.SendMsg,
		maxLoss:        maxLoss,
		recoverSuccess: recoverSuccess,
		onDown:         cfg.OnDown,
		onUp:           cfg.OnUp,
		observer:       cfg.Observer,
		onDelivery:     cfg.OnDelivery,
		pending:        make(map[uint64]uint64),
		dead:           cfg.InitDead,
	}
}

// Start actively emits ping messages at the configured interval until ctx is
// canceled. Each message is sent through the bound sendMsg callback.
func (p *Ping) Start(ctx context.Context) error {
	if p.sendMsg == nil {
		return errors.New("ping: no send callback configured")
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Send the first ping immediately
	if err := p.sendPing(time.Now()); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			if err := p.sendPing(now); err != nil {
				return err
			}
			p.cleanupExpired(now)
		}
	}
}

func (p *Ping) sendPing(now time.Time) error {
	nowMS := uint64(now.UnixMilli())

	p.mu.Lock()
	id := p.nextID
	p.nextID++
	p.pending[id] = nowMS
	p.mu.Unlock()

	return p.sendMsg(Message{ID: id, TimeMS: nowMS})
}

// cleanupExpired removes pings whose timeout has elapsed and accumulates
// liveness loss. Each expired ping is one consecutive loss (spec 5.5); when
// lossCount first reaches MaxLoss the path is declared dead and OnDown fires
// once. The callback is invoked outside the lock.
func (p *Ping) cleanupExpired(now time.Time) {
	if p.timeout == 0 {
		return
	}

	cutoff := uint64(now.Add(-p.timeout).UnixMilli())

	p.mu.Lock()
	fireDown := false
	expired := 0
	for id, sendMS := range p.pending {
		if sendMS < cutoff {
			delete(p.pending, id)
			expired++
			if p.markLostLocked() {
				fireDown = true
			}
		}
	}
	p.mu.Unlock()

	// Each expired ping is one failed delivery (spec 5.4). Report outside the lock.
	if p.onDelivery != nil {
		for i := 0; i < expired; i++ {
			p.onDelivery(false)
		}
	}
	if fireDown && p.onDown != nil {
		p.onDown()
	}
}

// markLostLocked records one consecutive loss and reports whether this loss is
// the transition into the dead state (so the caller fires OnDown exactly once).
// Caller holds p.mu. Mirrors old probe/core markLost semantics.
func (p *Ping) markLostLocked() bool {
	p.lossCount++
	p.recoverCount = 0
	if p.dead || p.lossCount < p.maxLoss {
		return false
	}
	p.dead = true
	return true
}

// Pong validates an incoming pong message against pending pings. If the ID and
// TimeMS match a pending ping, it updates RTT estimation, accumulates liveness
// recovery, and returns the updated Quality. The second return value is true
// when the pong was valid.
//
// Liveness (spec 5.5): a valid pong clears lossCount. If the path was dead,
// consecutive valid pongs accumulate recoverCount; on reaching RecoverSuccess
// the path is declared alive and OnUp fires once. The Observer callback (if set)
// receives every valid sample's Quality so the leg observer can track RTT.
//
// Invalid or duplicate pongs are dropped; Pong returns zero Quality and false.
func (p *Ping) Pong(pong Message, nowMS uint64) (Quality, bool) {
	p.mu.Lock()

	sendMS, ok := p.pending[pong.ID]
	if !ok {
		// Unknown or duplicate pong
		p.mu.Unlock()
		return Quality{}, false
	}

	// Validate that the pong's TimeMS matches what we sent
	if pong.TimeMS != sendMS {
		delete(p.pending, pong.ID)
		p.mu.Unlock()
		return Quality{}, false
	}

	delete(p.pending, pong.ID)

	// Calculate RTT sample
	if nowMS < sendMS {
		// Clock skew, drop the sample
		p.mu.Unlock()
		return Quality{}, false
	}

	sampleMS := uint32(nowMS - sendMS)
	p.lastSampleMS = sampleMS
	p.sampleCount++

	// Update smoothed RTT using RFC 6298 formulas
	if p.sampleCount == 1 {
		// First sample
		p.srtt = sampleMS
		p.rttVar = sampleMS / 2
	} else {
		// Subsequent samples: SRTT = (1-alpha) * SRTT + alpha * sample
		// RTTVAR = (1-beta) * RTTVAR + beta * |SRTT - sample|
		// Using alpha = 1/8, beta = 1/4
		const alpha = 8
		const beta = 4

		absDiff := func(a, b uint32) uint32 {
			if a > b {
				return a - b
			}
			return b - a
		}

		diff := absDiff(p.srtt, sampleMS)
		p.rttVar = (p.rttVar*(beta-1) + diff) / beta
		p.srtt = (p.srtt*(alpha-1) + sampleMS) / alpha
	}

	quality := Quality{
		SampleMS: sampleMS,
		SRTTMS:   p.srtt,
		RTTVarMS: p.rttVar,
		Samples:  p.sampleCount,
	}

	// Liveness recovery (mirrors old probe/core handlePONG semantics).
	fireUp := p.markRecoveredLocked()
	p.mu.Unlock()

	if fireUp && p.onUp != nil {
		p.onUp()
	}
	if p.observer != nil {
		p.observer(quality)
	}
	// A valid pong is one successful delivery (spec 5.4).
	if p.onDelivery != nil {
		p.onDelivery(true)
	}
	return quality, true
}

// MarkAlive synchronizes the ping's liveness state with an external transport
// proof, such as a HELLO_ACK received on the same leg. It does not fire OnUp:
// the caller already owns the corresponding leg activation.
func (p *Ping) MarkAlive() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.dead = false
	p.lossCount = 0
	p.recoverCount = 0
	p.mu.Unlock()
}

// markRecoveredLocked records one successful pong toward recovery and reports
// whether this pong is the transition back to alive (so the caller fires OnUp
// exactly once). Caller holds p.mu. Mirrors old probe/core handlePONG.
func (p *Ping) markRecoveredLocked() bool {
	p.lossCount = 0
	if !p.dead {
		p.recoverCount = 0
		return false
	}
	p.recoverCount++
	if p.recoverCount < p.recoverSuccess {
		return false
	}
	p.dead = false
	p.recoverCount = 0
	return true
}

// CurrentQuality returns the current RTT quality state without updating it.
func (p *Ping) CurrentQuality() Quality {
	p.mu.Lock()
	defer p.mu.Unlock()

	return Quality{
		SampleMS: p.lastSampleMS,
		SRTTMS:   p.srtt,
		RTTVarMS: p.rttVar,
		Samples:  p.sampleCount,
	}
}
