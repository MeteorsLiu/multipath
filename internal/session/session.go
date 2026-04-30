package session

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
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

func (s *Session) Open(nowMS uint64) *Hello {
	s.mu.Lock()
	defer s.mu.Unlock()
	nonce := s.nextNonce
	s.nextNonce++
	hello := &Hello{
		sessionID: s.id,
		nonce:     nonce,
		openedMS:  nowMS,
		pending:   true,
	}
	if s.hellos == nil {
		s.hellos = make(map[uint64]*Hello)
	}
	s.hellos[nonce] = hello
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

	hello.mu.Lock()
	hello.pending = false
	hello.mu.Unlock()
	return accepted
}

func (s *Session) Do(fn func(View) error) error {
	return fn(View{sessionID: s.id})
}

type Hello struct {
	sessionID uint64
	nonce     uint64
	openedMS  uint64

	mu      sync.Mutex
	pending bool
}

func (h *Hello) Do(fn func(View) error) error {
	return fn(View{sessionID: h.sessionID, nonce: h.nonce, hasNonce: true})
}

func (h *Hello) Retry(nowMS uint64, fn func(View) error) (sent bool, expired bool, err error) {
	if h == nil {
		return false, true, nil
	}
	h.mu.Lock()
	pending := h.pending
	h.mu.Unlock()
	if !pending {
		return false, true, nil
	}
	if err := h.Do(fn); err != nil {
		return false, false, err
	}
	return true, false, nil
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
