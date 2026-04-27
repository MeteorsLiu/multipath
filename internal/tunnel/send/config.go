package send

import (
	"time"

	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

type Config struct {
	StreamTransport transport.StreamTransport
	SessionManager  *sessionpkg.Manager
	ProbeInterval   time.Duration
	ProbeTimeout    time.Duration
	ProbeEvents     chan probe.Event
	EnableFEC       bool
	BootstrapLanes  []BootstrapLane
}

type BootstrapLane struct {
	SessionID uint64
	LaneID    uint8
	Weight    uint32
	Leg       transport.LegRef
	TCPRemote string
}
