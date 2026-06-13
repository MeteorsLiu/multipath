package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

var errZeroSessionID = errors.New("session: generated zero session id")

type Manager struct {
	mu       sync.RWMutex
	sessions map[uint64]*Session
}

func New() (*Session, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint64(buf[:])
	if id == 0 {
		return nil, errZeroSessionID
	}
	return &Session{id: id}, nil
}

func (m *Manager) Get(id uint64) (*Session, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.sessions == nil {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) Add(s *Session) bool {
	if m == nil || s == nil || s.id == 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[uint64]*Session)
	}
	if existing := m.sessions[s.id]; existing != nil {
		return false
	}
	m.sessions[s.id] = s
	return true
}

func (m *Manager) Create(id uint64) (*Session, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[uint64]*Session)
	}
	if s := m.sessions[id]; s != nil {
		return nil, false
	}
	s := &Session{id: id}
	m.sessions[id] = s
	return s, true
}

func (m *Manager) GetOrCreate(id uint64) (*Session, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[uint64]*Session)
	}
	if s := m.sessions[id]; s != nil {
		return s, true
	}
	s := &Session{id: id}
	m.sessions[id] = s
	return s, true
}

func (m *Manager) Delete(id uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		return
	}
	delete(m.sessions, id)
}

type Session struct {
	id uint64

	mu        sync.Mutex
	nextNonce uint64
	hellos    map[uint64]*Hello
}

// HelloConfig configures Hello retry behavior.
type HelloConfig struct {
	RetryInterval time.Duration
	MaxRetries    int
	TimeoutMS     uint64
}

// FrameSender sends one HELLO frame. The View carries the sessionID and nonce
// so the caller's closure can construct the HELLO frame; identity ("which leg
// this nonce is for") lives in the caller's closure, not in Hello. Injected by
// the caller (spec 5.1).
type FrameSender func(ctx context.Context, v View) error

// Open creates a new Hello that drives its own retry loop. The returned Hello
// starts a goroutine that sends HELLO frames via sender until Ack() is called
// or the retry limit/timeout is reached. onExpire is called if the Hello expires
// without receiving an ACK.
func (s *Session) Open(ctx context.Context, cfg HelloConfig, sender FrameSender, onExpire func()) *Hello {
	s.mu.Lock()
	nonce := s.nextNonce
	s.nextNonce++
	s.mu.Unlock()

	hctx, cancel := context.WithCancel(ctx)
	hello := &Hello{
		sessionID: s.id,
		nonce:     nonce,
		openedMS:  uint64(time.Now().UnixMilli()),
		ctx:       hctx,
		cancel:    cancel,
		sendFrame: sender,
		onExpire:  onExpire,
		cfg:       cfg,
		ackCh:     make(chan struct{}),
	}

	s.mu.Lock()
	if s.hellos == nil {
		s.hellos = make(map[uint64]*Hello)
	}
	s.hellos[nonce] = hello
	s.mu.Unlock()

	go hello.loop()
	return hello
}

func (s *Session) Ack(nonce uint64, accepted bool) bool {
	s.mu.Lock()
	hello := s.hellos[nonce]
	if hello == nil {
		s.mu.Unlock()
		return false
	}
	delete(s.hellos, nonce)
	s.mu.Unlock()

	if hello != nil {
		hello.Ack()
	}
	return accepted
}

func (s *Session) Do(fn func(View) error) error {
	return fn(View{sessionID: s.id})
}

type Hello struct {
	sessionID uint64
	nonce     uint64
	openedMS  uint64

	ctx       context.Context
	cancel    context.CancelFunc
	sendFrame FrameSender
	onExpire  func()
	cfg       HelloConfig
	ackCh     chan struct{}
	once      sync.Once
}

func (h *Hello) Do(fn func(View) error) error {
	return fn(View{sessionID: h.sessionID, nonce: h.nonce, hasNonce: true})
}

// Ack signals that a HELLO_ACK was received. It stops the retry loop.
func (h *Hello) Ack() {
	h.once.Do(func() {
		close(h.ackCh)
		h.cancel()
	})
}

// loop drives the HELLO retry mechanism until Ack() is called or the retry
// limit/timeout is reached.
func (h *Hello) loop() {
	ticker := time.NewTicker(h.cfg.RetryInterval)
	defer ticker.Stop()

	retries := 0
	startMS := h.openedMS

	for {
		// Send HELLO
		if err := h.sendFrame(h.ctx, View{sessionID: h.sessionID, nonce: h.nonce, hasNonce: true}); err != nil {
			// Log but continue retrying
		}
		retries++

		// Check limits
		nowMS := uint64(time.Now().UnixMilli())
		if h.cfg.MaxRetries > 0 && retries >= h.cfg.MaxRetries {
			h.expire()
			return
		}
		if h.cfg.TimeoutMS > 0 && nowMS-startMS >= h.cfg.TimeoutMS {
			h.expire()
			return
		}

		// Wait for ACK or next retry
		select {
		case <-h.ackCh:
			return
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			// Continue to next retry
		}
	}
}

func (h *Hello) expire() {
	h.cancel()
	if h.onExpire != nil {
		h.onExpire()
	}
}

type View struct {
	sessionID uint64
	nonce     uint64
	hasNonce  bool
}

func (v View) SessionID() uint64 {
	return v.sessionID
}

func (v View) Nonce() uint64 {
	if !v.hasNonce {
		return 0
	}
	return v.nonce
}
