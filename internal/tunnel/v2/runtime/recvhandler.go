// Package runtime holds the tunnel runtime glue that wires Send and Recv
// together (spec 5.6). The RecvHandler implements recv.Handler: it converts
// between protocol frames and semantic probe values, and drives all output
// through Send.WriteFrame. Session remains the HELLO authority.
//
// Active probing (the outbound PING/BW_PROBE drivers) lives in Send, which
// registers each ping/BwLoop in the shared LaneManager (spec 5.7). The recv glue
// only routes inbound replies: PONG → LaneManager.LookupPing(key).Pong,
// BW_PROBE_ACK → LaneManager.LookupBwLoop(id).Ack. It also answers inbound
// PING (bounce PONG) and HELLO (reply HELLO_ACK), and validates HELLO_ACK via
// Session. The recv glue never calls a Send transport method directly and never
// touches lane/leg internals.
//
// This is the only place that knows both protocol frames and the semantic probe
// packages; neither probe/ping nor probe/bw imports send or protocol, and Send
// itself stays thin (encode, schedule, lane send, FEC).
package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/protocol"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/bw"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/probe/ping"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/recv"
	"github.com/MeteorsLiu/multipath/internal/tunnel/v2/send"
)

// RecvHandler implements recv.Handler (spec 5.6). It routes each decoded control
// frame to the matching Session / probe / Send.WriteFrame action. Active probe
// instances live in the shared LaneManager (read via send.LaneManager()); the
// recv glue only looks them up to feed inbound replies.
type RecvHandler struct {
	send     *send.Send
	sessions *sessionpkg.Manager
	lanes    *send.LaneManager

	bwReferenceBps uint64
	bwCapBps       uint64
	qosWriter      *QoSWriter

	// receive is the passive bandwidth side (spec 5.8): it tracks per-train
	// received bitmaps and returns the Ack to send for each inbound probe. The
	// recv glue writes the returned Ack as a BW_PROBE_ACK frame. Active probing
	// (BwLoop) lives in send and is reached via the shared LaneManager.
	receive *bw.Receive

	passiveBWMu     sync.Mutex
	passiveBWTrains map[uint64]send.LegKey
	passiveBWTCPRef map[send.LaneKey]uint64
	passiveBWObs    map[uint64]recv.BandwidthProbeObservation
}

// Config holds optional probe tuning for the runtime glue.
type Config struct {
	BWReferenceBps uint64
	BWCapBps       uint64
	QoSWriter      *QoSWriter
}

// NewRecvHandler returns a recv.Handler backed by s and sessions. It shares s's
// LaneManager (spec 5.7) so inbound PONG / BW_ACK reach the send-side active
// probe instances.
func NewRecvHandler(s *send.Send, sessions *sessionpkg.Manager, configs ...Config) *RecvHandler {
	h := &RecvHandler{
		send:            s,
		sessions:        sessions,
		lanes:           s.LaneManager(),
		passiveBWTrains: make(map[uint64]send.LegKey),
		passiveBWTCPRef: make(map[send.LaneKey]uint64),
		passiveBWObs:    make(map[uint64]recv.BandwidthProbeObservation),
	}
	h.receive = bw.NewReceive(bw.ReceiveConfig{
		OnSample: h.onPassiveBWSample,
	})
	for _, cfg := range configs {
		if cfg.BWReferenceBps > 0 {
			h.bwReferenceBps = cfg.BWReferenceBps
		}
		if cfg.BWCapBps > 0 {
			h.bwCapBps = cfg.BWCapBps
		}
		if cfg.QoSWriter != nil {
			h.qosWriter = cfg.QoSWriter
		}
	}
	return h
}

// RecvHandler structurally satisfies the v2 recv.Handler interface (spec 5.6).
// The Handler methods take transport.LegRef, which recv.Ref aliases, so the
// match holds without any signature change.
var _ recv.Handler = (*RecvHandler)(nil)

// OnHello handles incoming HELLO frames (spec 7.3). Session is the HELLO
// authority: GetOrCreate the session, then reply with HELLO_ACK through
// Send.WriteFrame on the observed transport.
func (h *RecvHandler) OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.HelloBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}

	// Session owns the HELLO lifecycle.
	h.sessions.GetOrCreate(frame.SessionID)

	caps := uint16(0)
	fecProfile := protocol.FECProfileOff
	if h.send.FECEnabled() && body.Caps&protocol.CapFEC != 0 && body.FECProfile == protocol.FECProfileSLC4Plus1 {
		caps = protocol.CapFEC
		if body.Caps&protocol.CapLinkStatus != 0 {
			caps |= protocol.CapLinkStatus
		}
		fecProfile = protocol.FECProfileSLC4Plus1
	}

	ackFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeHELLOACK,
		SessionID: frame.SessionID,
		LaneID:    frame.LaneID,
		Body: protocol.HelloAckBody{
			Nonce:      body.Nonce,
			Accepted:   1,
			Caps:       caps,
			FECProfile: fecProfile,
		},
	}

	debuglog.Printf("runtime", "hello session=%d lane=%d", frame.SessionID, frame.LaneID)
	if err := h.send.WriteFrame(ctx, ackFrame, leg); err != nil {
		return err
	}
	if caps&protocol.CapLinkStatus != 0 && h.qosWriter != nil {
		h.qosWriter.Enable(frame.SessionID, frame.LaneID)
	}
	return nil
}

// OnHelloAck handles incoming HELLO_ACK frames (spec 7.3). Session.Ack validates
// the nonce and stops the Hello retry loop; send registers the OnAck closure
// that activates the leg. The recv glue does not touch lane/leg internals.
func (h *RecvHandler) OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.HelloAckBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}

	sess, ok := h.sessions.Get(frame.SessionID)
	if !ok {
		return nil
	}

	accepted := sess.Ack(body.Nonce, body.Accepted == 1)
	if !accepted {
		debuglog.Printf("runtime", "hello_ack_rejected session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}

	if h.send.FECEnabled() && body.Caps&protocol.CapFEC != 0 && body.FECProfile == protocol.FECProfileSLC4Plus1 {
		h.send.EnableFEC()
	}
	if body.Caps&protocol.CapLinkStatus != 0 && body.Caps&protocol.CapFEC != 0 && h.qosWriter != nil {
		h.qosWriter.Enable(frame.SessionID, frame.LaneID)
	}

	debuglog.Printf("runtime", "hello_ack session=%d lane=%d kind=%d", frame.SessionID, frame.LaneID, leg.Kind)
	return nil
}

// sessionKnown reports whether the session exists. When it does not — e.g. the
// peer restarted and is probing a session this end has forgotten, or this end
// restarted and the peer is still using the old session id — the caller must
// not silently answer (a bare PONG/ACK would let the peer believe the dead
// session is alive forever). Instead it tells the peer the session is gone via
// closeUnknownSession (spec: 重启→重连 自愈).
func (h *RecvHandler) sessionKnown(sessionID uint64) bool {
	_, ok := h.sessions.Get(sessionID)
	return ok
}

// closeUnknownSession replies CLOSE{Session, UnknownSession} on the observed
// transport, telling the peer this end does not know the session so it can tear
// down and rebootstrap. Mirrors the old writeUnknownSessionClose.
func (h *RecvHandler) closeUnknownSession(ctx context.Context, leg transport.LegRef, sessionID uint64) error {
	debuglog.Printf("runtime", "close_unknown_session session=%d kind=%d", sessionID, leg.Kind)
	return h.send.WriteFrame(ctx, protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeCLOSE,
		SessionID: sessionID,
		LaneID:    protocol.SessionControlLaneID,
		Body: protocol.CloseBody{
			Scope:  protocol.CloseScopeSession,
			Reason: protocol.CloseReasonUnknownSession,
		},
	}, leg)
}

// OnPing handles incoming PING frames (spec 7.4). The runtime glue builds a
// PONG with the same ID and TimeMS and writes it on the observed transport.
// probe/ping is not involved in the reply. If the session is unknown (peer using
// a stale session id after a restart), it replies CLOSE{UnknownSession} instead
// of a bare PONG so the peer can rebootstrap.
func (h *RecvHandler) OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}

	if !h.sessionKnown(frame.SessionID) {
		return h.closeUnknownSession(ctx, leg, frame.SessionID)
	}

	pongFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypePONG,
		SessionID: frame.SessionID,
		LaneID:    frame.LaneID,
		Body: protocol.PingBody{
			PingID: body.PingID,
			TimeMS: body.TimeMS,
		},
	}
	return h.send.WriteFrame(ctx, pongFrame, leg)
}

// OnPong handles incoming PONG frames (spec 7.4). It looks up the send-side
// active ping for this leg in the shared LaneManager and feeds it the reply.
// The ping owns liveness (OnUp/OnDown closures registered by send) and RTT; the
// recv glue does not touch lane/leg state. A PONG for a path we never probed
// (no registered ping) is dropped.
func (h *RecvHandler) OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.PingBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}

	if !h.sessionKnown(frame.SessionID) {
		return h.closeUnknownSession(ctx, leg, frame.SessionID)
	}

	p := h.lanes.LookupPing(send.KeyForLeg(frame.SessionID, frame.LaneID, leg))
	if p == nil {
		debuglog.Printf("runtime", "pong_drop no_ping session=%d lane=%d", frame.SessionID, frame.LaneID)
		return nil
	}

	msg := ping.Message{ID: body.PingID, TimeMS: body.TimeMS}
	nowMS := uint64(time.Now().UnixMilli())
	quality, ok := p.Pong(msg, nowMS)
	if !ok {
		return nil
	}

	debuglog.Printf("runtime", "pong session=%d lane=%d srtt=%dms var=%dms",
		frame.SessionID, frame.LaneID, quality.SRTTMS, quality.RTTVarMS)
	return nil
}

// OnClose handles incoming CLOSE frames (spec 7). A session-scope CLOSE deletes
// the session. When the reason is UnknownSession — the peer restarted and no
// longer knows our session — the send side rebuilds a fresh session (a new HELLO
// handshake) so the link self-heals instead of staying wedged on a dead session.
func (h *RecvHandler) OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.CloseBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	debuglog.Printf("runtime", "close session=%d lane=%d scope=%d reason=%d",
		frame.SessionID, frame.LaneID, body.Scope, body.Reason)

	if body.Scope != protocol.CloseScopeSession {
		return nil
	}

	// Only act on a CLOSE for the session we currently hold; a stale CLOSE for an
	// already-gone session must not trigger a rebuild (avoids CLOSE loops).
	if _, known := h.sessions.GetOrDelete(frame.SessionID); !known {
		return nil
	}

	if body.Reason == protocol.CloseReasonUnknownSession {
		eventlog.Printf("reconnect", "action=unknown_session_close session=%d leg=%s", frame.SessionID, runtimeKindLabel(leg.Kind))
		// The peer forgot our session (it restarted). Rebuild a fresh one. Only
		// the client (with bootstrap lanes) acts; the server side is a no-op and
		// just awaits the peer's new HELLO. Rebootstrap tears down the old session
		// state itself.
		if err := h.send.Rebootstrap(); err != nil {
			debuglog.Printf("runtime", "rebootstrap_err session=%d err=%v", frame.SessionID, err)
		}
		return nil
	}

	// Plain session CLOSE: release the send-side per-session state so it does not
	// leak under session churn (spec 7).
	h.send.CloseSession(frame.SessionID)
	return nil
}

// OnBandwidthProbe handles incoming BW_PROBE frames (spec 7.5). It feeds the
// probe to the passive Receive, which returns the Ack to send (and whether one
// is due now); the recv glue writes that Ack as a BW_PROBE_ACK frame. Observed
// BW_PROBE frames arm the passive-side bwScheduler through LaneManager; when the
// peer's train is done (TrainBytesRemaining==0), RemoteComplete releases the
// scheduler's remote phase.
func (h *RecvHandler) OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) (recv.BandwidthProbeObservation, error) {
	body, ok := frame.Body.(protocol.BandwidthProbeBody)
	if !ok {
		return recv.BandwidthProbeObservation{}, protocol.ErrInvalidFrame
	}

	if !h.sessionKnown(frame.SessionID) {
		return recv.BandwidthProbeObservation{}, h.closeUnknownSession(ctx, leg, frame.SessionID)
	}

	key := send.KeyForLeg(frame.SessionID, frame.LaneID, leg)
	h.rememberPassiveBWTrain(body.TrainID, key)
	h.lanes.RemoteProbe(key)

	probe := bw.Probe{
		TrainID:   body.TrainID,
		ID:        body.ProbeID,
		Seq:       body.Seq,
		Count:     body.Count,
		SendMS:    body.SendMS,
		TargetBps: body.TargetBps,
		Remaining: body.TrainBytesRemaining,
		Bytes:     len(body.Payload),
	}
	ack, shouldAck := h.receive.Probe(probe)
	observation := h.takePassiveBWObservation(body.TrainID)

	// Peer's train finished: release the gate's remote phase (spec 7.5).
	if body.TrainBytesRemaining == 0 {
		h.lanes.RemoteComplete(key)
	}

	if !shouldAck {
		return observation, nil
	}
	ackFrame := protocol.Frame{
		Version:   protocol.Version,
		Type:      protocol.TypeBandwidthProbeAck,
		SessionID: frame.SessionID,
		LaneID:    frame.LaneID,
		Body: protocol.BandwidthProbeAckBody{
			ProbeID:   ack.ID,
			Count:     ack.Count,
			Received:  ack.Received,
			FirstRXMS: ack.FirstRXMS,
			LastRXMS:  ack.LastRXMS,
		},
	}
	return observation, h.send.WriteFrame(ctx, ackFrame, leg)
}

func (h *RecvHandler) rememberPassiveBWTrain(trainID uint64, key send.LegKey) {
	if trainID == 0 {
		return
	}
	h.passiveBWMu.Lock()
	h.passiveBWTrains[trainID] = key
	h.passiveBWMu.Unlock()
}

func (h *RecvHandler) takePassiveBWObservation(trainID uint64) recv.BandwidthProbeObservation {
	if trainID == 0 {
		return recv.BandwidthProbeObservation{}
	}
	h.passiveBWMu.Lock()
	defer h.passiveBWMu.Unlock()
	observation := h.passiveBWObs[trainID]
	delete(h.passiveBWObs, trainID)
	return observation
}

func (h *RecvHandler) onPassiveBWSample(trainID uint64, sample bw.Sample) {
	h.passiveBWMu.Lock()
	key, ok := h.passiveBWTrains[trainID]
	if ok {
		delete(h.passiveBWTrains, trainID)
	}
	if !ok {
		h.passiveBWMu.Unlock()
		return
	}

	laneKey := send.LaneKey{SessionID: key.SessionID, LaneID: key.LaneID}
	if key.Kind == transport.KindTCP {
		if sample.BandwidthBps > 0 {
			h.passiveBWTCPRef[laneKey] = sample.BandwidthBps
		}
		h.passiveBWMu.Unlock()
		debuglog.Printf("runtime", "bw_passive_sample session=%d lane=%d kind=%s bps=%d loss=%.3f",
			key.SessionID, key.LaneID, runtimeKindLabel(key.Kind), sample.BandwidthBps, sample.Loss)
		return
	}

	tcpBps := h.passiveBWTCPRef[laneKey]
	capBps := sample.TargetBps
	if capBps == 0 {
		capBps = h.bwCapBps
	}
	referenceBps := sample.TargetBps
	if referenceBps == 0 {
		referenceBps = h.bwReferenceBps
		if referenceBps == 0 {
			if capBps > 0 {
				referenceBps = capBps
			} else {
				referenceBps = tcpBps
				capBps = tcpBps
			}
		}
	} else if capBps == 0 {
		capBps = referenceBps
	}
	h.passiveBWMu.Unlock()

	if key.Kind != transport.KindUDP {
		return
	}
	sample.ReferenceBps = referenceBps
	preferTCP := passiveBWPreferTCP(sample)
	primary := transport.KindUDP
	if preferTCP {
		primary = transport.KindTCP
	}
	debuglog.Printf("runtime", "bw_passive_sample session=%d lane=%d kind=%s bps=%d loss=%.3f reference_bps=%d prefer_tcp=%t primary=%s",
		key.SessionID, key.LaneID, runtimeKindLabel(key.Kind), sample.BandwidthBps, sample.Loss, sample.ReferenceBps, preferTCP, runtimeKindLabel(primary))
	eventlog.Printf("bw", "action=passive_sample session=%d lane=%d leg=%s bps=%d loss=%.3f reference_bps=%d prefer_tcp=%t selected_primary=%s",
		key.SessionID, key.LaneID, runtimeKindLabel(key.Kind), sample.BandwidthBps, sample.Loss, sample.ReferenceBps, preferTCP, runtimeKindLabel(primary))
	tcpBetter := sample.ReferenceBps > sample.BandwidthBps
	eventlog.Printf("bandwidth_probe_decision", "side=recv session=%d lane=%d udp_bps=%d udp_loss=%.3f tcp_bps=%d reference_bps=%d cap_bps=%d tcp_better=%t prefer_tcp=%t selected_leg=%s",
		key.SessionID, key.LaneID,
		sample.BandwidthBps, sample.Loss,
		tcpBps, sample.ReferenceBps, capBps,
		tcpBetter, preferTCP, runtimeKindLabel(primary))
	if trainID != 0 {
		h.passiveBWMu.Lock()
		h.passiveBWObs[trainID] = recv.BandwidthProbeObservation{
			Primary:         primary,
			UDPLimited:      preferTCP,
			UDPDeliveredBps: uint32Bps(sample.BandwidthBps),
			TCPDeliveredBps: uint32Bps(sample.ReferenceBps),
			CapBps:          uint32Bps(capBps),
		}
		h.passiveBWMu.Unlock()
	}
}

func uint32Bps(v uint64) uint32 {
	if v > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(v)
}

const (
	passiveBWLossThreshold  = 0.01
	passiveBWTCPPreferRatio = 3.0 / 2.0
)

func passiveBWPreferTCP(sample bw.Sample) bool {
	if sample.Loss >= passiveBWLossThreshold {
		return true
	}
	if sample.ReferenceBps > 0 && float64(sample.BandwidthBps)*passiveBWTCPPreferRatio <= float64(sample.ReferenceBps) {
		return true
	}
	return false
}

// OnBandwidthProbeAck handles incoming BW_PROBE_ACK frames (spec 7.5). It routes
// the ack to the active BwLoop for that train via the shared LaneManager; the
// loop folds it into its current step and (when the train completes) emits the
// Sample through its onSample closure (which feeds the observer + advances the
// gate). A BW_ACK for a train we are not actively probing is dropped.
func (h *RecvHandler) OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.BandwidthProbeAckBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}

	loop := h.lanes.LookupBwLoop(body.ProbeID)
	if loop == nil {
		debuglog.Printf("runtime", "bw_ack_drop no_loop session=%d lane=%d probe_id=%d",
			frame.SessionID, frame.LaneID, body.ProbeID)
		return nil
	}

	loop.Ack(bw.Ack{
		ID:        body.ProbeID,
		Count:     body.Count,
		Received:  body.Received,
		FirstRXMS: body.FirstRXMS,
		LastRXMS:  body.LastRXMS,
	})
	return nil
}

func (h *RecvHandler) OnQoS(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error {
	body, ok := frame.Body.(protocol.LinkStatusBody)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	if !h.sessionKnown(frame.SessionID) {
		return h.closeUnknownSession(ctx, leg, frame.SessionID)
	}
	udpLimited, tcpLimited, repairCount, ok := decodeLinkStatus(body.Status)
	if !ok {
		return protocol.ErrInvalidFrame
	}
	qos := h.lanes.LookupQoS(send.LaneKey{SessionID: frame.SessionID, LaneID: frame.LaneID})
	if qos == nil {
		debuglog.Printf("runtime", "link_status_drop no_qos session=%d lane=%d repair_count=%d", frame.SessionID, frame.LaneID, repairCount)
		recordLinkStatusEvent("apply_drop_no_qos", frame.SessionID, frame.LaneID, udpLimited, tcpLimited)
		return nil
	}
	qos.OnQoSStatus(udpLimited, body.UDPDeliveredBps, tcpLimited, body.TCPDeliveredBps, repairCount)
	debuglog.Printf("runtime", "link_status_apply session=%d lane=%d status=%#02x control_leg=%s udp_limited=%t udp_delivered_bps=%d tcp_limited=%t tcp_delivered_bps=%d repair_count=%d",
		frame.SessionID, frame.LaneID, body.Status, runtimeKindLabel(leg.Kind), udpLimited, body.UDPDeliveredBps, tcpLimited, body.TCPDeliveredBps, repairCount)
	recordLinkStatusEvent("apply", frame.SessionID, frame.LaneID, udpLimited, tcpLimited)
	return nil
}

func decodeLinkStatus(status uint8) (bool, bool, uint8, bool) {
	udpLimited, udpRepairCount, ok := decodeLinkStatusState(status >> 4)
	if !ok {
		return false, false, 0, false
	}
	tcpLimited, tcpRepairCount, ok := decodeLinkStatusState(status & 0x0f)
	if !ok || tcpRepairCount != udpRepairCount {
		return false, false, 0, false
	}
	return udpLimited, tcpLimited, udpRepairCount, true
}

func decodeLinkStatusState(state uint8) (bool, uint8, bool) {
	if state > 7 {
		return false, 0, false
	}
	return state&protocol.LinkStatusStateLimited != 0, (state >> 1) + 1, true
}
