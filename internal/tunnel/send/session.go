package send

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
)

// sendState holds per-session send-side state. The monotonic counters
// nextPacketID / nextRepairKey are atomic so the data path can reserve DATA and
// REPAIR identifiers without taking the mutex; mu is taken only when mutating
// the FEC tx window.
type sendState struct {
	nextPacketID  atomic.Uint32
	nextRepairKey atomic.Uint32 // upper 16 bits unused; only low 16 bits encoded

	mu            sync.Mutex
	txWindow      *txSLCWindow
	fecFlushTimer *time.Timer
	fecFlushArmed bool
}

// reservePacketID reserves and returns the next DATA packet id. Gaps are
// acceptable when a later send fails; duplicate packet ids are not.
func (s *sendState) reservePacketID() uint32 {
	return s.nextPacketID.Add(1) - 1
}

// commitPacket adds the packet to the FEC window when requested. Returns the
// repair group when a window completes.
func (s *sendState) commitPacket(packetID uint32, packet []byte, addToWindow bool) (txRepairGroup, bool, bool) {
	if !addToWindow || s.txWindow == nil {
		return txRepairGroup{}, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	wasEmpty := len(s.txWindow.pending) == 0
	group, ready := s.txWindow.add(packetID, packet)
	if ready {
		s.cancelFECFlushTimerLocked()
		return group, true, false
	}
	shouldArmFlush := wasEmpty && len(s.txWindow.pending) > 0
	return txRepairGroup{}, false, shouldArmFlush
}

// reserveRepairKey reserves and returns the next repair key.
func (s *sendState) reserveRepairKey() uint16 {
	return uint16(s.nextRepairKey.Add(1) - 1)
}

type fecCodec interface {
	Encode(shards [][]byte, key uint16) error
	Reconstruct(shards [][]byte, key uint16) error
}

// getSessionState looks up an existing send-side session state. The
// sessionStatesMu read lock is held for the map lookup only.
func (l *Send) getSessionState(sessionID uint64) (*sessionpkg.Session, *sendState, bool) {
	session, ok := l.sessionManager.Get(sessionID)
	if !ok {
		return nil, nil, false
	}
	l.sessionStatesMu.RLock()
	state := l.sendStates[session]
	l.sessionStatesMu.RUnlock()
	return session, state, state != nil
}

// getOrCreateSessionState resolves the *sessionpkg.Session for sessionID and
// installs a fresh sendState entry on first use. The fast path uses
// Manager.Get (RLock) and only falls back to Manager.GetOrCreate (Lock) when
// the session has not yet been admitted.
func (l *Send) getOrCreateSessionState(sessionID uint64) (*sessionpkg.Session, *sendState, bool) {
	session, ok := l.sessionManager.Get(sessionID)
	if !ok {
		session, ok = l.sessionManager.GetOrCreate(sessionID)
		if !ok {
			return nil, nil, false
		}
	}
	state, ok := l.getOrCreateSendState(session)
	return session, state, ok
}

func (l *Send) getOrCreateSendState(session *sessionpkg.Session) (*sendState, bool) {
	sessionID, ok := sessionIDOf(session)
	if !ok {
		return nil, false
	}
	if existing, ok := l.sessionManager.Get(sessionID); ok {
		if existing != session {
			return nil, false
		}
	} else if !l.sessionManager.Add(session) {
		return nil, false
	}

	l.sessionStatesMu.RLock()
	state := l.sendStates[session]
	l.sessionStatesMu.RUnlock()
	if state != nil {
		return state, true
	}

	l.sessionStatesMu.Lock()
	defer l.sessionStatesMu.Unlock()
	if state := l.sendStates[session]; state != nil {
		return state, true
	}
	state = &sendState{
		txWindow: newTxSLCWindow(4),
	}
	l.sendStates[session] = state
	debuglog.Printf("send", "session_create session=%d", sessionID)
	return state, true
}

// deleteSessionState removes the send-side state for a session, cancels any
// HELLO routes associated with it, and deletes the session from the manager.
func (l *Send) deleteSessionState(sessionID uint64) {
	session, ok := l.sessionManager.Get(sessionID)
	if ok {
		l.sessionStatesMu.Lock()
		state := l.sendStates[session]
		delete(l.sendStates, session)
		delete(l.strategies, sessionID)
		l.sessionStatesMu.Unlock()

		if state != nil {
			state.mu.Lock()
			state.cancelFECFlushTimerLocked()
			if state.txWindow != nil {
				state.txWindow.releaseAll()
			}
			state.mu.Unlock()
		}

		l.helloRoutesMu.Lock()
		var pending []uint64
		for key, route := range l.helloRoutes {
			if key.sessionID != sessionID {
				continue
			}
			pending = append(pending, route.nonce())
			delete(l.helloRoutes, key)
		}
		l.helloRoutesMu.Unlock()

		for _, nonce := range pending {
			session.Ack(nonce, false)
		}
	}
	l.sessionManager.Delete(sessionID)
}

func (l *Send) enableFEC() {
	l.fecProfile.Store(uint32(protocol.FECProfileSLCVariablePlus1))
	for sourceSpan := 1; sourceSpan <= maxFECSourceSpan; sourceSpan++ {
		if l.fecCodecs[sourceSpan] == nil {
			l.fecCodecs[sourceSpan], _ = fecpkg.NewCodec(sourceSpan, 1)
		}
	}
	if l.fecCodec == nil {
		l.fecCodec = l.fecCodecs[maxFECSourceSpan]
	}
}
