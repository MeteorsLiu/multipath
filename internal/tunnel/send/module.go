package send

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	"github.com/MeteorsLiu/multipath/internal/scheduler"
	"github.com/MeteorsLiu/multipath/internal/scheduler/cfs"
	"github.com/MeteorsLiu/multipath/internal/transport"
	probe "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

var (
	errNoRunnableLane = errors.New("tunnel: no runnable lane")
	errUnknownLane    = errors.New("tunnel: scheduler returned unknown lane")
	errInvalidLane    = errors.New("tunnel: invalid lane")
)

type Send struct {
	mu               sync.Mutex
	activeSessionID  uint64
	hasActiveSession bool
	probeInterval    time.Duration
	probeTimeout     time.Duration
	nextNonce        uint64
	nextProbeTarget  probe.Target
	lanes            map[laneKey]*laneRuntime
	sessions         map[uint64]*sessionRuntime
	schedulers       map[uint64]scheduler.Scheduler
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
		lanes:      make(map[laneKey]*laneRuntime),
		sessions:   make(map[uint64]*sessionRuntime),
		schedulers: make(map[uint64]scheduler.Scheduler),
		packets:    make(chan transport.Payload, defaultPacketQueueSize),
	}
	for _, cfg := range configs {
		in.applyConfig(cfg)
	}
	return in
}

func (l *Send) applyConfig(cfg Config) {
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
	l.bootstrapLanes = append(l.bootstrapLanes, cfg.BootstrapLanes...)
}

func (l *Send) Packets() <-chan transport.Payload {
	return l.packets
}

func (l *Send) bootstrapLocked(ctx context.Context) error {
	if l.bootstrapped {
		return nil
	}
	l.bootstrapped = true

	for _, lane := range l.bootstrapLanes {
		nonce := lane.Nonce
		if nonce == 0 {
			nonce = l.nextNonce
			l.nextNonce++
		}
		caps := protocol.SupportedCaps
		fecProfile := protocol.FECProfileOff
		if lane.EnableFEC {
			caps |= protocol.CapFEC
			fecProfile = protocol.FECProfileSLC4Plus1
		}
		if err := l.startLane(ctx, startLaneConfig{
			SessionID:  lane.SessionID,
			LaneID:     lane.LaneID,
			Weight:     lane.Weight,
			Leg:        lane.Leg,
			TCPRemote:  lane.TCPRemote,
			Nonce:      nonce,
			Caps:       caps,
			FECProfile: fecProfile,
		}); err != nil {
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

func (l *Send) scheduler(sessionID uint64) scheduler.Scheduler {
	sched := l.schedulers[sessionID]
	if sched != nil {
		return sched
	}

	sched = cfs.New()
	l.schedulers[sessionID] = sched
	return sched
}

func (l *Send) enqueueLane(sessionID uint64, lane *laneRuntime) error {
	if lane == nil || lane.queued {
		return nil
	}
	if err := l.scheduler(sessionID).Enqueue(lane.id, lane.weight, 0); err != nil {
		return err
	}
	lane.queued = true
	return nil
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
	return l.WriteTo(ctx, leg, packet)
}

func (l *Send) enqueueFrame(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (int, error) {
	packet, err := l.encodePacket(frame)
	if err != nil {
		return 0, err
	}
	size := len(packet.Payload)
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
