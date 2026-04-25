package send

import (
	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
)

type sessionRuntime struct {
	sessionID      uint64
	nextPacketID   uint32
	nextRepairKey  uint16
	negotiatedCaps uint16
	fecProfile     uint8
	fecCodec       fecCodec
	txWindow       *txSLCWindow
}

type fecCodec interface {
	Encode(shards [][]byte, key uint16) error
	Reconstruct(shards [][]byte, key uint16) error
}

func (l *Send) session(sessionID uint64) *sessionRuntime {
	session := l.sessions[sessionID]
	if session != nil {
		return session
	}

	session = &sessionRuntime{
		sessionID: sessionID,
		txWindow:  newTxSLCWindow(4),
	}
	session.fecCodec, _ = fecpkg.NewCodec(4, 1)
	l.sessions[sessionID] = session
	debuglog.Printf("send", "session_create session=%d fec_codec=%t", sessionID, session.fecCodec != nil)
	return session
}
