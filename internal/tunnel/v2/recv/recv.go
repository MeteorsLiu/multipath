// Package recv is the v2 receive side. It decodes transport-bound frames,
// handles DATA and REPAIR locally with per-lane FEC receive windows and a
// session-scoped emit dedupe, dispatches control frames to a recv.Handler
// (spec 5.5/5.6), and runs the receive-side FEC differential QoS detector.
//
// recv imports neither send nor the probe packages; the runtime glue that wires
// Recv to Send.WriteFrame lives elsewhere.
package recv

import (
	"context"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	fecpkg "github.com/MeteorsLiu/multipath/internal/fec"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultPacketQueueSize = 1024
	maxFECSourceSpan       = 4
)

// Handler receives the control frames Recv does not handle locally (spec 5.6).
// DATA and REPAIR never reach a Handler. The from argument is the observed
// transport (spec design name Ref; an alias of transport.LegRef).
type Handler interface {
	OnHello(ctx context.Context, from Ref, frame protocol.Frame) error
	OnHelloAck(ctx context.Context, from Ref, frame protocol.Frame) error
	OnPing(ctx context.Context, from Ref, frame protocol.Frame) error
	OnPong(ctx context.Context, from Ref, frame protocol.Frame) error
	OnClose(ctx context.Context, from Ref, frame protocol.Frame) error
	OnBandwidthProbe(ctx context.Context, from Ref, frame protocol.Frame) error
	OnBandwidthProbeAck(ctx context.Context, from Ref, frame protocol.Frame) error
	OnQoS(ctx context.Context, from Ref, frame protocol.Frame) error
}

type QoSStatus struct {
	SessionID       uint64
	LaneID          uint8
	UDPLimited      bool
	TCPLimited      bool
	RepairCount     uint8
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
}

type QoSCallback func(ctx context.Context, status QoSStatus) error

type Config struct {
	Handler        Handler
	SessionManager *sessionpkg.Manager
	OnQoSStatus    QoSCallback
}

// Recv decodes transport-bound frames into IP packets and dispatches control
// frames to its Handler.
//
// Locking convention:
//
//   - statesMu (RWMutex) protects the states map and is also held while calling
//     sessionManager.Get/Delete to keep the "session admitted iff state
//     present" invariant under concurrent close.
//   - recvState.mu protects the per-session windows, QoS estimators, and closed
//     flag. It is taken AFTER releasing statesMu, never the other way around.
type Recv struct {
	statesMu  sync.RWMutex
	handler   Handler
	manager   *sessionpkg.Manager
	onQoS     QoSCallback
	fecCodecs [maxFECSourceSpan + 1]fecCodec
	packets   chan *packetbuf.Packet
	states    map[*sessionpkg.Session]*recvState
}

type recvState struct {
	mu        sync.Mutex
	sessionID uint64
	closed    bool

	// rxWindows holds one FEC receive window per lane id. DATA and REPAIR select
	// their window by frame.LaneID, so reconstruction never crosses lanes
	// (spec 9.2).
	rxWindows map[uint8]*rxGroupWindow

	// dedupe is the session-scoped emit dedupe (spec 8.2). It decides whether an
	// original or FEC-recovered packet id has already been written to TUN.
	dedupe *emitDedupe

	// qos holds the receive-side FEC differential estimator per lane. It consumes
	// already-aggregated lane DATA/FEC samples, then emits QoS status through
	// Recv's callback.
	qos map[uint8]*qosEstimator

	fecGroups map[rxLaneGroupKey]rxGroupObservation
	fecTimers map[rxLaneGroupKey]*time.Timer

	// shardScratch is reused by recoverPacket to materialize the shard slice for
	// fec.Reconstruct without per-call allocation.
	shardScratch     [8][]byte
	repairKeyScratch [4]uint16
}

type rxLaneGroupKey struct {
	laneID uint8
	group  rxGroupKey
}

type rxGroupObservation struct {
	dataKind   transport.Kind
	repairKind transport.Kind
}

// windowFor returns the per-lane FEC receive window for laneID, creating it on
// first use. Caller holds s.mu.
func (s *recvState) windowFor(laneID uint8) *rxGroupWindow {
	w := s.rxWindows[laneID]
	if w == nil {
		w = newRxGroupWindow()
		s.rxWindows[laneID] = w
	}
	return w
}

func (o *Recv) qosFor(state *recvState, laneID uint8) *qosEstimator {
	if state == nil {
		return nil
	}
	sessionID := state.sessionID
	q := state.qos[laneID]
	if q == nil {
		q = newQoSEstimator(qosConfig{
			SessionID: sessionID,
			LaneID:    laneID,
			AutoStart: true,
		}, func(status qosStatus) {
			if err := o.reportQoS(context.Background(), sessionID, laneID, []qosStatus{status}); err != nil {
				debuglog.Printf("recv", "qos_report_error session=%d lane=%d err=%v", sessionID, laneID, err)
			}
		})
		state.qos[laneID] = q
	}
	return q
}

func (o *Recv) observeDataRate(state *recvState, laneID uint8, dataKind transport.Kind, dataBytes int, at time.Time) []qosStatus {
	if !qosKnownKind(dataKind) || dataBytes <= 0 {
		return nil
	}
	repairKind := otherTransportKind(dataKind)
	if !validQoSDirection(dataKind, repairKind) {
		return nil
	}
	return o.qosFor(state, laneID).ObserveRate(qosRateSample{
		At:         at,
		DataKind:   dataKind,
		RepairKind: repairKind,
		DataBytes:  uint64(dataBytes),
	})
}

func (o *Recv) observeRepairRate(state *recvState, laneID uint8, repairKind transport.Kind, sourceSpan, repairBytes int, at time.Time) []qosStatus {
	if sourceSpan <= 0 || repairBytes <= 0 || !qosKnownKind(repairKind) {
		return nil
	}
	dataKind := otherTransportKind(repairKind)
	if !validQoSDirection(dataKind, repairKind) {
		return nil
	}
	return o.qosFor(state, laneID).ObserveRate(qosRateSample{
		At:                 at,
		DataKind:           dataKind,
		RepairKind:         repairKind,
		RepairBytes:        uint64(repairBytes),
		ProfileDataBytes:   uint64(sourceSpan * repairBytes),
		ProfileRepairBytes: uint64(repairBytes),
	})
}

func (o *Recv) trackRepairGroup(state *recvState, laneID uint8, group rxGroupKey, repairKind transport.Kind) {
	key := rxLaneGroupKey{laneID: laneID, group: group}
	dataKind := otherTransportKind(repairKind)
	if validQoSDirection(dataKind, repairKind) {
		state.fecGroups[key] = rxGroupObservation{dataKind: dataKind, repairKind: repairKind}
	}
	if timer := state.fecTimers[key]; timer != nil {
		timer.Stop()
	}
	state.fecTimers[key] = time.AfterFunc(defaultQoSGroupMature, func() {
		o.expireFECGroup(state, laneID, group)
	})
}

func (o *Recv) observeGroupResult(state *recvState, laneID uint8, result rxGroupWindowResult, at time.Time) []qosStatus {
	var statuses []qosStatus
	for _, done := range result.done {
		key := rxLaneGroupKey{laneID: laneID, group: done.group}
		o.cancelFECGroupTimer(state, key)
		obs, ok := state.fecGroups[key]
		delete(state.fecGroups, key)
		if !ok || done.dataExpected == 0 {
			continue
		}
		if status, changed := o.qosFor(state, laneID).ObserveHealth(qosHealthSample{
			At:           at,
			DataKind:     obs.dataKind,
			RepairKind:   obs.repairKind,
			DataArrived:  uint64(done.dataArrived),
			DataExpected: uint64(done.dataExpected),
		}); changed {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func (o *Recv) expireFECGroup(state *recvState, laneID uint8, group rxGroupKey) {
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return
	}
	window := state.rxWindows[laneID]
	if window == nil {
		state.mu.Unlock()
		return
	}
	result := window.expireGroup(group)
	statuses := o.observeGroupResult(state, laneID, result, time.Now())
	sessionID := state.sessionID
	state.mu.Unlock()
	if err := o.reportQoS(context.Background(), sessionID, laneID, statuses); err != nil {
		debuglog.Printf("recv", "qos_report_error session=%d lane=%d err=%v", sessionID, laneID, err)
	}
}

func (o *Recv) cancelFECGroupTimer(state *recvState, key rxLaneGroupKey) {
	if timer := state.fecTimers[key]; timer != nil {
		timer.Stop()
	}
	delete(state.fecTimers, key)
}

type fecCodec interface {
	Reconstruct(shards [][]byte, keys []uint16) error
}

func New(configs ...Config) *Recv {
	out := &Recv{
		states:  make(map[*sessionpkg.Session]*recvState),
		packets: make(chan *packetbuf.Packet, defaultPacketQueueSize),
		manager: &sessionpkg.Manager{},
	}
	for sourceSpan := 1; sourceSpan <= maxFECSourceSpan; sourceSpan++ {
		out.fecCodecs[sourceSpan], _ = fecpkg.NewCodec(sourceSpan, 1)
	}
	for _, cfg := range configs {
		if cfg.Handler != nil {
			out.handler = cfg.Handler
		}
		if cfg.SessionManager != nil {
			out.manager = cfg.SessionManager
		}
		if cfg.OnQoSStatus != nil {
			out.onQoS = cfg.OnQoSStatus
		}
	}
	return out
}

// Packets exposes the TUN-bound IP packet stream. The caller owns each packet
// and must Release it after writing to TUN.
func (o *Recv) Packets() <-chan *packetbuf.Packet {
	return o.packets
}

func (o *Recv) Write(ctx context.Context, packet *packetbuf.Packet) error {
	return o.WriteTo(ctx, Ref{}, packet)
}

// WriteTo decodes one transport-bound packet observed on leg and routes it.
func (o *Recv) WriteTo(ctx context.Context, leg Ref, packet *packetbuf.Packet) error {
	if packet == nil {
		return nil
	}
	releaseEventPacket := true
	defer func() {
		if releaseEventPacket {
			packet.Release()
		}
	}()

	frame, err := protocol.Decode(packet.Payload)
	if err != nil {
		if debuglog.Enabled() {
			debuglog.Printf("recv", "decode_drop leg={%s} bytes=%d err=%v", debugLeg(leg), len(packet.Payload), err)
		}
		metrics.IncCounter(metrics.ProtocolDecodeErrorsTotal,
			metrics.L("leg", kindMetricLabel(leg.Kind)),
		)
		return nil
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv", "frame_in %s leg={%s}", debugFrameSummary(frame), debugLeg(leg))
	}
	metrics.IncCounter(metrics.ProtocolFramesTotal,
		metrics.LStr("direction", "rx"),
		metrics.LStr("type", debugFrameType(frame.Type)),
		metrics.LU64("session", frame.SessionID),
		metrics.LU8("lane", frame.LaneID),
		metrics.LStr("leg", kindMetricLabel(leg.Kind)),
	)

	switch frame.Type {
	case protocol.TypeHELLO:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeHELLOACK:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypePING:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypePONG:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeDATA:
		consumed, err := o.handleDATA(ctx, leg, frame, packet)
		if consumed {
			releaseEventPacket = false
		}
		return err
	case protocol.TypeREPAIR:
		return o.handleREPAIR(ctx, leg, frame)
	case protocol.TypeCLOSE:
		return o.handleCLOSE(ctx, leg, frame)
	case protocol.TypeBandwidthProbe:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeBandwidthProbeAck:
		return o.handleControl(ctx, leg, frame)
	case protocol.TypeLinkStatus:
		return o.handleControl(ctx, leg, frame)
	default:
		return nil
	}
}

// handleControl validates a control frame's body matches its type, then
// dispatches it to the matching Handler method. DATA and REPAIR never reach
// here. A nil Handler drops the frame.
func (o *Recv) handleControl(ctx context.Context, leg Ref, frame protocol.Frame) error {
	if err := validateControlBody(frame); err != nil {
		return err
	}
	if o.handler == nil {
		return nil
	}
	switch frame.Type {
	case protocol.TypeHELLO:
		return o.handler.OnHello(ctx, leg, frame)
	case protocol.TypeHELLOACK:
		return o.handler.OnHelloAck(ctx, leg, frame)
	case protocol.TypePING:
		return o.handler.OnPing(ctx, leg, frame)
	case protocol.TypePONG:
		return o.handler.OnPong(ctx, leg, frame)
	case protocol.TypeBandwidthProbe:
		return o.handler.OnBandwidthProbe(ctx, leg, frame)
	case protocol.TypeBandwidthProbeAck:
		return o.handler.OnBandwidthProbeAck(ctx, leg, frame)
	case protocol.TypeLinkStatus:
		return o.handler.OnQoS(ctx, leg, frame)
	default:
		return nil
	}
}

// validateControlBody returns ErrInvalidFrame when the decoded body type does
// not match the frame type.
func validateControlBody(frame protocol.Frame) error {
	var ok bool
	switch frame.Type {
	case protocol.TypeHELLO:
		_, ok = frame.Body.(protocol.HelloBody)
	case protocol.TypeHELLOACK:
		_, ok = frame.Body.(protocol.HelloAckBody)
	case protocol.TypePING, protocol.TypePONG:
		_, ok = frame.Body.(protocol.PingBody)
	case protocol.TypeBandwidthProbe:
		_, ok = frame.Body.(protocol.BandwidthProbeBody)
	case protocol.TypeBandwidthProbeAck:
		_, ok = frame.Body.(protocol.BandwidthProbeAckBody)
	case protocol.TypeLinkStatus:
		_, ok = frame.Body.(protocol.LinkStatusBody)
	default:
		ok = true
	}
	if !ok {
		return protocol.ErrInvalidFrame
	}
	return nil
}

// handleDATA implements spec 8.3. It returns true when ownership of packet was
// transferred to the TUN output channel (zero-copy emit).
func (o *Recv) handleDATA(ctx context.Context, leg Ref, frame protocol.Frame, packet *packetbuf.Packet) (bool, error) {
	body, ok := frame.Body.(protocol.DataBody)
	if !ok {
		return false, protocol.ErrInvalidFrame
	}

	state := o.recvState(frame.SessionID)
	if state == nil {
		return false, nil
	}

	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return false, nil
	}
	window := state.windowFor(frame.LaneID)
	if !state.dedupe.mark(body.PacketID) {
		state.mu.Unlock()
		if debuglog.Enabled() {
			debuglog.Printf("recv", "data_drop_duplicate session=%d packet_id=%d", frame.SessionID, body.PacketID)
		}
		return false, nil
	}
	now := time.Now()
	result := window.addData(body.PacketID, body.Packet)
	statuses := o.observeDataRate(state, frame.LaneID, leg.Kind, len(body.Packet), now)
	statuses = append(statuses, o.observeGroupResult(state, frame.LaneID, result, now)...)
	state.mu.Unlock()
	if err := o.reportQoS(ctx, frame.SessionID, frame.LaneID, statuses); err != nil {
		return false, err
	}

	consumed, err := o.emitTransportPacket(ctx, packet, body.Packet)
	if err != nil {
		return false, err
	}
	if len(result.recoverable) > 0 {
		return consumed, o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, result.recoverable[0])
	}
	return consumed, nil
}

// handleREPAIR implements spec 8.4.
func (o *Recv) handleREPAIR(ctx context.Context, leg Ref, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.RepairBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	if body.SourceSpan == 0 || body.SourceSpan > maxFECSourceSpan {
		return nil
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_accept"),
		metrics.L("session", frame.SessionID),
		metrics.L("source_span", body.SourceSpan),
	)

	state := o.recvState(frame.SessionID)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return nil
	}
	window := state.windowFor(frame.LaneID)
	now := time.Now()
	group := rxGroupKey{basePacketID: body.BasePacketID, sourceSpan: int(body.SourceSpan)}
	_, closed := window.closed[group]
	if !closed {
		o.trackRepairGroup(state, frame.LaneID, group, leg.Kind)
	}
	result := window.addRepair(body.BasePacketID, body.Key, int(body.SourceSpan), body.Symbol)
	var statuses []qosStatus
	if !closed {
		statuses = o.observeRepairRate(state, frame.LaneID, leg.Kind, int(body.SourceSpan), len(body.Symbol), now)
	}
	statuses = append(statuses, o.observeGroupResult(state, frame.LaneID, result, now)...)
	state.mu.Unlock()
	if err := o.reportQoS(ctx, frame.SessionID, frame.LaneID, statuses); err != nil {
		return err
	}
	if len(result.recoverable) > 0 {
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "repair_recoverable"),
			metrics.L("session", frame.SessionID),
			metrics.L("source_span", result.recoverable[0].group.sourceSpan),
		)
		return o.maybeRecover(ctx, frame.SessionID, frame.LaneID, state, result.recoverable[0])
	}
	return nil
}

func (o *Recv) reportQoS(ctx context.Context, sessionID uint64, laneID uint8, statuses []qosStatus) error {
	if o.onQoS == nil {
		return nil
	}
	for _, status := range statuses {
		if err := o.onQoS(ctx, QoSStatus{
			SessionID:       sessionID,
			LaneID:          laneID,
			UDPLimited:      status.UDPLimited,
			TCPLimited:      status.TCPLimited,
			UDPDeliveredBps: status.UDPDeliveredBps,
			TCPDeliveredBps: status.TCPDeliveredBps,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (o *Recv) handleCLOSE(ctx context.Context, leg Ref, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	var session *sessionpkg.Session
	if body.Scope == protocol.CloseScopeSession {
		session, _ = o.manager.Get(frame.SessionID)
	}
	if o.handler != nil {
		if err := o.handler.OnClose(ctx, leg, frame); err != nil {
			return err
		}
	}
	if body.Scope == protocol.CloseScopeSession {
		o.closeRecvState(frame.SessionID, session)
	}
	return nil
}

func (o *Recv) maybeRecover(ctx context.Context, sessionID uint64, laneID uint8, state *recvState, recoverable rxGroupRecoverable) error {
	if countMissing(recoverable.missingMask, recoverable.group.sourceSpan) != 1 {
		return nil
	}
	codec := o.fecCodecForSourceSpan(recoverable.group.sourceSpan)
	if state == nil || codec == nil {
		return nil
	}
	pkt, statuses, ok := o.recoverPacket(sessionID, laneID, state, recoverable, codec)
	if err := o.reportQoS(ctx, sessionID, laneID, statuses); err != nil {
		if pkt != nil {
			pkt.Release()
		}
		return err
	}
	if !ok {
		return nil
	}
	select {
	case o.packets <- pkt:
		return nil
	case <-ctx.Done():
		pkt.Release()
		return ctx.Err()
	}
}

func (o *Recv) recoverPacket(sessionID uint64, laneID uint8, state *recvState, recoverable rxGroupRecoverable, codec fecCodec) (*packetbuf.Packet, []qosStatus, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, nil, false
	}
	window := state.rxWindows[laneID]
	if window == nil {
		return nil, nil, false
	}
	shards, repairKeys, ok := window.buildShardsLocked(recoverable, state.shardScratch[:0], state.repairKeyScratch[:0])
	if !ok {
		return nil, nil, false
	}
	if len(repairKeys) != 1 {
		return nil, nil, false
	}
	if err := codec.Reconstruct(shards, repairKeys); err != nil {
		debuglog.Printf("recv", "recover_err session=%d lane=%d base_packet_id=%d key=%d source_span=%d err=%v",
			sessionID, laneID, recoverable.group.basePacketID, repairKeys[0], recoverable.group.sourceSpan, err)
		metrics.IncCounter(metrics.FECEventsTotal,
			metrics.L("event", "recover_err"),
			metrics.L("session", sessionID),
			metrics.L("source_span", recoverable.group.sourceSpan),
		)
		return nil, nil, false
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "reconstruct_done"),
		metrics.L("session", sessionID),
		metrics.L("source_span", recoverable.group.sourceSpan),
	)

	missingIndex := firstMissingIndex(recoverable.missingMask, recoverable.group.sourceSpan)
	if missingIndex < 0 {
		return nil, nil, false
	}
	packetID := recoverable.group.basePacketID + uint32(missingIndex)
	reconstructed := shards[missingIndex]
	payload, ipOK := recoveredIPv4Packet(reconstructed)
	if !ipOK {
		return nil, nil, false
	}
	result := window.finishRecovery(recoverable)
	statuses := o.observeGroupResult(state, laneID, result, time.Now())
	if !state.dedupe.mark(packetID) {
		return nil, statuses, false
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "recover_emit"),
		metrics.L("session", sessionID),
		metrics.L("source_span", recoverable.group.sourceSpan),
	)
	debuglog.Printf("recv", "recover_emit session=%d lane=%d packet_id=%d base_packet_id=%d key=%d source_span=%d bytes=%d",
		sessionID, laneID, packetID, recoverable.group.basePacketID, repairKeys[0], recoverable.group.sourceSpan, len(payload))
	pkt := packetbuf.Acquire(len(payload))
	copy(pkt.Payload, payload)
	return pkt, statuses, true
}

func (o *Recv) fecCodecForSourceSpan(sourceSpan int) fecCodec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return nil
	}
	return o.fecCodecs[sourceSpan]
}

func (o *Recv) emitTransportPacket(ctx context.Context, packet *packetbuf.Packet, payload []byte) (bool, error) {
	packet.Payload = payload
	select {
	case o.packets <- packet:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (o *Recv) recvState(sessionID uint64) *recvState {
	o.statesMu.RLock()
	session, ok := o.manager.Get(sessionID)
	if !ok {
		o.statesMu.RUnlock()
		return nil
	}
	state := o.states[session]
	o.statesMu.RUnlock()
	if state != nil {
		return state
	}

	o.statesMu.Lock()
	defer o.statesMu.Unlock()
	session, ok = o.manager.Get(sessionID)
	if !ok {
		return nil
	}
	if state := o.states[session]; state != nil {
		return state
	}
	state = &recvState{
		sessionID: sessionID,
		rxWindows: make(map[uint8]*rxGroupWindow),
		dedupe:    newEmitDedupe(0),
		qos:       make(map[uint8]*qosEstimator),
		fecGroups: make(map[rxLaneGroupKey]rxGroupObservation),
		fecTimers: make(map[rxLaneGroupKey]*time.Timer),
	}
	o.states[session] = state
	debuglog.Printf("recv", "session_create session=%d", sessionID)
	return state
}

func (o *Recv) closeRecvState(sessionID uint64, session *sessionpkg.Session) {
	o.statesMu.Lock()
	if session == nil {
		session, _ = o.manager.GetOrDelete(sessionID)
	} else {
		o.manager.Delete(sessionID)
	}
	var state *recvState
	if session != nil {
		state = o.states[session]
		delete(o.states, session)
	}
	o.statesMu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.closed = true
	for _, q := range state.qos {
		q.Close()
	}
	for key, timer := range state.fecTimers {
		if timer != nil {
			timer.Stop()
		}
		delete(state.fecTimers, key)
	}
	for key := range state.fecGroups {
		delete(state.fecGroups, key)
	}
	for _, window := range state.rxWindows {
		window.releaseAll()
	}
	state.mu.Unlock()
}

func recoveredIPv4Packet(packet []byte) ([]byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil, false
	}
	headerLen := int(packet[0]&0x0f) * 4
	totalLen := int(packet[2])<<8 | int(packet[3])
	if headerLen < 20 || totalLen < headerLen || totalLen > len(packet) {
		return nil, false
	}
	return packet[:totalLen], true
}
