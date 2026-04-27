package send

import (
	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
)

type sendState struct {
	nextPacketID  uint32
	nextRepairKey uint16
	txWindow      *txSLCWindow
}

type fecCodec interface {
	Encode(shards [][]byte, key uint16) error
	Reconstruct(shards [][]byte, key uint16) error
}

func (l *Send) getSessionState(sessionID uint64) (*sessionpkg.Session, *sendState, bool) {
	session, ok := l.sessionManager.Get(sessionID)
	if !ok {
		return nil, nil, false
	}
	state := l.sendStates[session]
	return session, state, state != nil
}

func (l *Send) getOrCreateSessionState(sessionID uint64) (*sessionpkg.Session, *sendState, bool) {
	session, ok := l.sessionManager.GetOrCreate(sessionID)
	if !ok {
		return nil, nil, false
	}
	state := l.sendStates[session]
	if state != nil {
		return session, state, true
	}

	state = &sendState{
		txWindow: newTxSLCWindow(4),
	}
	l.sendStates[session] = state
	debuglog.Printf("send", "session_create session=%d", sessionID)
	return session, state, true
}

func (l *Send) deleteSessionState(sessionID uint64) {
	if session, ok := l.sessionManager.Get(sessionID); ok {
		delete(l.sendStates, session)
		for key, route := range l.helloRoutes {
			if key.sessionID != sessionID {
				continue
			}
			session.Ack(route.nonce(), false)
			delete(l.helloRoutes, key)
		}
	}
	l.sessionManager.Delete(sessionID)
}

func (l *Send) enableFEC() {
	l.fecProfile = protocol.FECProfileSLC4Plus1
	if l.fecCodec == nil {
		l.fecCodec, _ = fecpkg.NewCodec(4, 1)
	}
}
