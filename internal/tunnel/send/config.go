package send

import (
	"time"

	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

type Config struct {
	StreamTransport     transport.StreamTransport
	SessionManager      *sessionpkg.Manager
	ProbeInterval       time.Duration
	ProbeTimeout        time.Duration
	ProbeEvents         chan probe.Event
	EnableFEC           bool
	FECFlushAlpha       uint32
	FECFlushMinMs       uint32
	FECFlushMaxMs       uint32
	FECFlushColdStartMs uint32
	FECFlushFixedMs     uint32
	BootstrapLanes      []BootstrapLane
}

type BootstrapLane struct {
	LaneID    uint8
	Weight    uint32
	Leg       transport.LegRef
	TCPRemote string
}
