package send

import (
	"time"

	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

// Config holds the configuration for creating a Send instance.
//
// Send is thin (spec 5.2): it owns encode, schedule, lane send, and FEC. It also
// creates and drives the active ping per lane (spec 5.5/7.3), registering each in
// the shared LaneManager (spec 5.7). Probe interval/timeout tune that ping.
type Config struct {
	SessionManager *sessionpkg.Manager
	// LaneManager is the shared probe-state seam between send and the recv glue
	// (spec 5.7). When nil, Send creates its own; wire the same instance into the
	// recv glue so inbound PONG / BW_ACK reach the send-side probe instances.
	LaneManager *LaneManager
	// StreamTransport dials TCP legs (spec 5.6). When set, a bootstrap lane with a
	// TCPRemote starts a dialer that establishes + redials the TCP leg off the data
	// path. nil disables TCP (UDP-only).
	StreamTransport transport.StreamTransport
	EnableFEC       bool

	// ProbeInterval/ProbeTimeout tune the active ping (spec 5.5). Zero falls back
	// to ping defaults.
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration

	// Bandwidth probing (spec 5.8). IsClient marks the end that sent HELLO; it
	// starts the gate in the Local phase (the peer starts in Remote). BWCapBps>0
	// restricts probing to UDP and caps the rate. BWReferenceBps seeds the UDP
	// rate adaptation. When EnableBandwidthProbe is false, no bwScheduler runs.
	IsClient             bool
	BWCapBps             uint64
	BWReferenceBps       uint64
	EnableBandwidthProbe bool

	BootstrapLanes []BootstrapLane
}

// BootstrapLane defines a lane to be created at bootstrap time.
type BootstrapLane struct {
	LaneID    uint8
	Weight    uint32
	Leg       transport.LegRef
	TCPRemote string
}
