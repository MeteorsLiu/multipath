package send

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/schedule"
	"github.com/MeteorsLiu/multipath/internal/schedule/cfs"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

var (
	errNoRunnableLane = errors.New("tunnel: no runnable lane")
	errUnknownLane    = errors.New("tunnel: unknown lane")
	errInvalidLane    = errors.New("tunnel: invalid lane")
)

type Send struct {
	mu               sync.Mutex
	activeSessionID  uint64
	hasActiveSession bool
	probeInterval    time.Duration
	probeTimeout     time.Duration
	nextProbeTarget  probe.Target
	negotiatedCaps   uint16
	fecProfile       uint8
	fecCodec         fecCodec
	lanes            map[laneKey]*laneRuntime
	sessionManager   *sessionpkg.Manager
	sendStates       map[*sessionpkg.Session]*sendState
	runnableCaches   map[uint64]*runnableLaneCache
	helloRoutes      map[laneKey]helloRoute
	strategies       map[uint64]schedule.Strategy[*laneRuntime]
	probeTargets     map[probe.Target]probeBinding
	probeKeys        map[pingKey]probe.Target
	bootstrapLanes   []BootstrapLane
	bootstrapped     bool
	probeEvents      chan probe.Event
	packets          chan transport.Payload

	streamTransport transport.StreamTransport
}

func New(configs ...Config) *Send {
	in := &Send{
		lanes:          make(map[laneKey]*laneRuntime),
		sendStates:     make(map[*sessionpkg.Session]*sendState),
		runnableCaches: make(map[uint64]*runnableLaneCache),
		helloRoutes:    make(map[laneKey]helloRoute),
		strategies:     make(map[uint64]schedule.Strategy[*laneRuntime]),
		packets:        make(chan transport.Payload, defaultPacketQueueSize),
		sessionManager: &sessionpkg.Manager{},
	}
	for _, cfg := range configs {
		in.applyConfig(cfg)
	}
	return in
}

func (l *Send) applyConfig(cfg Config) {
	if cfg.SessionManager != nil {
		l.sessionManager = cfg.SessionManager
	}
	if cfg.StreamTransport != nil {
		l.streamTransport = cfg.StreamTransport
	}
	if cfg.ProbeInterval > 0 {
		l.probeInterval = cfg.ProbeInterval
	}
	if cfg.ProbeTimeout > 0 {
		l.probeTimeout = cfg.ProbeTimeout
	}
	if cfg.ProbeEvents != nil {
		l.probeEvents = cfg.ProbeEvents
	}
	if cfg.EnableFEC {
		l.enableFEC()
	}
	l.bootstrapLanes = append(l.bootstrapLanes, cfg.BootstrapLanes...)
}

func (l *Send) Packets() <-chan transport.Payload {
	return l.packets
}

func (l *Send) bootstrapLocked(ctx context.Context) error {
	if l.bootstrapped {
		debuglog.Printf("send", "bootstrap_skip already_bootstrapped")
		return nil
	}
	l.bootstrapped = true
	debuglog.Printf("send", "bootstrap lanes=%d", len(l.bootstrapLanes))

	for _, lane := range l.bootstrapLanes {
		caps := protocol.CapTCPFallback
		fecProfile := l.fecProfile
		if fecProfile == protocol.FECProfileSLC4Plus1 {
			caps |= protocol.CapFEC
		}
		debuglog.Printf("send", "bootstrap_lane session=%d lane=%d weight=%d leg={%s} tcp_remote=%s caps=%#x fec_profile=%d", lane.SessionID, lane.LaneID, lane.Weight, debugLeg(lane.Leg), lane.TCPRemote, caps, fecProfile)
		if err := l.startLane(ctx, startLaneConfig{
			SessionID:  lane.SessionID,
			LaneID:     lane.LaneID,
			Weight:     lane.Weight,
			Leg:        lane.Leg,
			TCPRemote:  lane.TCPRemote,
			Caps:       caps,
			FECProfile: fecProfile,
		}); err != nil {
			debuglog.Printf("send", "bootstrap_lane_err session=%d lane=%d err=%v", lane.SessionID, lane.LaneID, err)
			return err
		}
	}
	return nil
}

func (l *Send) activateSession(sessionID uint64) {
	l.activeSessionID = sessionID
	l.hasActiveSession = true
}

func (l *Send) encodePacket(frame protocol.Frame) (*packetbuf.Packet, error) {
	size, err := frameEncodeCapacity(frame)
	if err != nil {
		return nil, err
	}
	packet := packetbuf.Acquire(size)
	encoded, err := protocol.Encode(frame, packet.Payload[:0])
	if err != nil {
		packet.Release()
		return nil, err
	}
	packet.Payload = encoded
	return packet, nil
}

func (l *Send) strategy(sessionID uint64) schedule.Strategy[*laneRuntime] {
	strategy := l.strategies[sessionID]
	if strategy != nil {
		return strategy
	}

	strategy = cfs.New[*laneRuntime]()
	l.strategies[sessionID] = strategy
	return strategy
}

func (l *Send) pickLane(sessionID uint64, cost uint32) (*laneRuntime, bool) {
	lanes := l.runnableLanes(sessionID)
	if len(lanes) == 0 {
		return nil, false
	}
	return l.strategy(sessionID).Pick(lanes, cost)
}

func (l *Send) runnableLanes(sessionID uint64) []*laneRuntime {
	cache := l.runnableCaches[sessionID]
	if cache != nil && !cache.dirty {
		return cache.lanes
	}
	if cache == nil {
		cache = &runnableLaneCache{dirty: true}
		l.runnableCaches[sessionID] = cache
	}
	lanes := cache.lanes[:0]
	for key, lane := range l.lanes {
		if key.sessionID == sessionID && lane.ready() {
			lanes = append(lanes, lane)
		}
	}
	sort.Slice(lanes, func(i, j int) bool {
		return lanes[i].id < lanes[j].id
	})
	cache.lanes = lanes
	cache.dirty = false
	return lanes
}

func (l *Send) markRunnableLanesDirty(sessionID uint64) {
	cache := l.runnableCaches[sessionID]
	if cache == nil {
		cache = &runnableLaneCache{dirty: true}
		l.runnableCaches[sessionID] = cache
		return
	}
	cache.dirty = true
}

func (l *Send) deleteRunnableLanesCache(sessionID uint64) {
	delete(l.runnableCaches, sessionID)
}

type runnableLaneCache struct {
	lanes []*laneRuntime
	dirty bool
}

type laneKey struct {
	sessionID uint64
	laneID    uint8
}

type fallbackDialResult struct {
	key laneKey
	leg transport.LegRef
	err error
}

type probeBinding struct {
	sessionID uint64
	laneID    uint8
	leg       transport.LegRef
}

const defaultPacketQueueSize = 1024

func (l *Send) enqueuePayload(ctx context.Context, leg transport.LegRef, payload []byte) error {
	packet := packetbuf.Acquire(len(payload))
	copy(packet.Payload, payload)
	packet.SetLen(len(payload))
	debuglog.Printf("send", "enqueue_payload leg={%s} bytes=%d", debugLeg(leg), len(payload))
	return l.WriteTo(ctx, leg, packet)
}

func (l *Send) enqueueFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (int, error) {
	packet, err := l.encodePacket(frame)
	if err != nil {
		debuglog.Printf("send", "enqueue_frame_encode_err frame=%s err=%v", debugFrameSummary(frame), err)
		return 0, err
	}
	size := len(packet.Payload)
	debuglog.Printf("send", "enqueue_frame frame=%s leg={%s} bytes=%d", debugFrameSummary(frame), debugLeg(leg), size)
	metrics.IncCounter(metrics.ProtocolFramesTotal,
		metrics.L("direction", "tx"),
		metrics.L("type", debugFrameType(frame.Type)),
		metrics.L("session", frame.SessionID),
		metrics.L("lane", frame.LaneID),
		metrics.L("leg", kindMetricLabel(leg.Kind)),
	)
	return size, l.WriteTo(ctx, leg, packet)
}

func frameEncodeCapacity(frame protocol.Frame) (int, error) {
	const headerSize = 10
	switch frame.Type {
	case protocol.TypeHELLO:
		_, ok := frame.Body.(protocol.HelloBody)
		return headerSize + 11, validFrameBody(ok)
	case protocol.TypeHELLOACK:
		_, ok := frame.Body.(protocol.HelloAckBody)
		return headerSize + 12, validFrameBody(ok)
	case protocol.TypePING, protocol.TypePONG:
		_, ok := frame.Body.(protocol.PingBody)
		return headerSize + 16, validFrameBody(ok)
	case protocol.TypeDATA:
		body, ok := frame.Body.(protocol.DataBody)
		if !ok {
			return 0, protocol.ErrInvalidFrame
		}
		return headerSize + 4 + len(body.Packet), nil
	case protocol.TypeREPAIR:
		body, ok := frame.Body.(protocol.RepairBody)
		if !ok {
			return 0, protocol.ErrInvalidFrame
		}
		return headerSize + 6 + len(body.Symbol), nil
	case protocol.TypeCLOSE:
		_, ok := frame.Body.(protocol.CloseBody)
		return headerSize + 2, validFrameBody(ok)
	default:
		return 0, protocol.ErrInvalidFrame
	}
}

func validFrameBody(ok bool) error {
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return nil
}
