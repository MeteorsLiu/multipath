package send

import (
	"time"

	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

type Config struct {
	StreamTransport transport.StreamTransport
	ProbeInterval   time.Duration
	ProbeTimeout    time.Duration
	ProbeEvents     chan probe.Event
	BootstrapLanes  []BootstrapLane
}

type BootstrapLane struct {
	SessionID uint64
	LaneID    uint8
	Weight    uint32
	Leg       transport.LegRef
	TCPRemote string
	Nonce     uint64
	EnableFEC bool
}
