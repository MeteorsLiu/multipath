# Multipath Tunnel Architecture

This document describes the current v2 runtime module boundaries for the
multipath tunnel. The wire format is described in [protocol.md](protocol.md).

## Module Boundaries

Public runtime modules:

```text
Send
Recv
Runtime RecvHandler
Session
Schedule Strategy
Transport
Protocol
FEC
TUN I/O adapter
Probe packages
```

There is no public Tunnel module, tunnel facade, public Lane module, public Path
module, or shared runtime-data module between Send and Recv. Lane and transport
leg runtime state are internal to Send. Runtime glue constructs modules
explicitly, starts loops, and waits for cancellation or errors.

Current tunnel runtime implementation packages live under `internal/tunnel/v2`:

```text
internal/tunnel/v2/send
internal/tunnel/v2/recv
internal/tunnel/v2/runtime
internal/tunnel/v2/probe/ping
internal/tunnel/v2/probe/bw
```

The former non-v2 `internal/tunnel/send`, `internal/tunnel/recv`,
`internal/tunnel/runtime`, and `internal/tunnel/probe` packages have been
removed. Do not import or recreate them for compatibility.

## Construction

The application wires the v2 runtime explicitly:

```go
sessions := &session.Manager{}

sender := send.New(send.Config{
    StreamTransport:      streamTransport,
    SessionManager:       sessions,
    ProbeInterval:        probeInterval,
    ProbeTimeout:         probeTimeout,
    IsClient:             isClient,
    EnableBandwidthProbe: true,
    BWCapBps:             bwCapBps,
    BWReferenceBps:       bwReferenceBps,
    BootstrapLanes:       bootstrap,
})
if enableFEC {
    sender.EnableFEC()
}

qosWriter := runtime.NewQoSWriter(sender)
handler := runtime.NewRecvHandler(sender, sessions, runtime.Config{
    QoSWriter: qosWriter,
})
receiver := recv.New(recv.Config{
    Handler:        handler,
    SessionManager: sessions,
    OnQoSStatus:    qosWriter.Write,
})
```

The runtime glue then starts:

```text
tun.Run(ctx, tunReader, sender)
transport.RunWriter(ctx, sender.Packets(), packetTransport, streamTransport)
packetTransport.Run(ctx, receiver)
streamTransport.Run(ctx, receiver)
tun.RunWriter(ctx, receiver.Packets(), tunWriter)
metricsServer.Run(ctx)
```

Send starts its own bootstrap HELLO retry loops, active ping loops, TCP dialers,
and optional bandwidth-probe scheduler from `Send.Bootstrap(ctx)`. There is no
separate public ProbeLoop in v2.

## Data Flow

TUN packet to transport:

```text
TUN
 -> tun.Run
 -> Send.Write(packet)
 -> active session id
 -> DRR Schedule Strategy Pick()
 -> DATA frame on selected lane primary transport
 -> optional lane-local REPAIR on selected lane shadow transport
 -> Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Transport payload to TUN:

```text
UDP/TCP Transport
 -> Recv.WriteTo(observed leg, packet)
 -> Protocol.Decode
 -> DATA/REPAIR handled inside Recv
 -> Recv.Packets()
 -> tun.RunWriter
 -> TUN
```

Transport control payload:

```text
UDP/TCP Transport
 -> Recv.WriteTo(observed leg, packet)
 -> Protocol.Decode
 -> RecvHandler method
 -> Session / probe package / Send.WriteFrame
 -> Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Recv handles control-frame classification and must not expose a control-frame
channel. DATA and REPAIR never go to `recv.Handler`. HELLO and HELLO_ACK state
transitions go through Session; protocol frame construction stays in Send or
runtime callbacks and does not move into Session.

## Send

Role:

```text
TUN packet ingress, send-side lane scheduling, transport-leg lifecycle, active
probe drivers, and transport-bound packet queue
```

Owns:

```text
active outbound session id
lane runtime data
per-session DRR Schedule Strategy
outbound packet id allocator
DATA frame construction
per-lane FEC transmit windows
transport-bound output queue
bootstrap HELLO send/retry callbacks
active ping instances registered in LaneManager
optional TCP dialers and TCP leg failure handling
optional bandwidth-probe scheduler
local FEC capability and per-session FEC enabled state
```

Interface sketch:

```go
package send

type Config struct {
    SessionManager       *session.Manager
    LaneManager          *LaneManager
    StreamTransport      transport.StreamTransport
    ProbeInterval        time.Duration
    ProbeTimeout         time.Duration
    IsClient             bool
    BWCapBps             uint64
    BWReferenceBps       uint64
    EnableBandwidthProbe bool
    BootstrapLanes       []BootstrapLane
}

type BootstrapLane struct {
    LaneID    uint8
    Weight    uint32
    Leg       transport.LegRef
    TCPRemote string
}

func New(configs ...Config) *Send
func (s *Send) Bootstrap(ctx context.Context) error
func (s *Send) Rebootstrap() error
func (s *Send) CloseSession(sessionID uint64)
func (s *Send) LaneManager() *LaneManager
func (s *Send) FECEnabled() bool
func (s *Send) EnableFEC()
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error
func (s *Send) WriteFrame(ctx context.Context, frame protocol.Frame, to transport.LegRef) error
func (s *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error
func (s *Send) Packets() <-chan transport.Payload
func (s *Send) OnLegFailure(ctx context.Context, leg transport.LegRef, err error)
```

Rules:

```text
Send does not read TUN.
Send does not read transport sockets.
Send is the only module that schedules TUN DATA packets across lanes.
Send.Write takes ownership of the TUN packet and releases it before returning.
Send.WriteFrame sends a caller-constructed protocol frame on an explicit
transport ref, or with zero ref via the target lane's control transport policy.
WriteFrame does not run schedule strategy lane selection.
Send.WriteTo takes ownership of an already encoded transport payload and emits
it through Send.Packets().
Send.Packets returns transport-bound packets. The consumer releases each packet
after the transport write returns.
Send must not expose broad semantic control wrappers such as AcceptHello,
AcceptHelloAck, ReceivePing, ReceivePong, or ReceiveBandwidthProbe.
Send may expose narrow runtime seams needed by transport failure handling,
rebootstrap, FEC capability, and the shared probe-state LaneManager.
```

Lane runtime is internal to Send. Each lane owns UDP/TCP leg refs and liveness,
primary/shadow transport selection, and a lane-local FEC transmit window. DATA
uses the lane primary transport. REPAIR uses the lane shadow transport. If only
one transport is active, both DATA and REPAIR use that transport as the
single-leg degradation case.

FEC is enabled in two steps: `EnableFEC()` marks local capability and initializes
codecs; data-plane REPAIR is emitted only for sessions whose HELLO/HELLO_ACK
negotiation accepted FEC.

## Recv

Role:

```text
transport payload ingress, protocol decode, local DATA/REPAIR handling, and
control dispatch
```

Interface sketch:

```go
package recv

type Handler interface {
    OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnBandwidthProbe(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnBandwidthProbeAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnQoS(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
}

type QoSStatus struct {
    SessionID       uint64
    LaneID          uint8
    UDPLimited      bool
    TCPLimited      bool
    UDPDeliveredBps uint32
    TCPDeliveredBps uint32
}

type QoSCallback func(ctx context.Context, status QoSStatus) error

type Config struct {
    Handler        Handler
    SessionManager *session.Manager
    OnQoSStatus    QoSCallback
}

func New(configs ...Config) *Recv
func (r *Recv) Write(ctx context.Context, packet *packetbuf.Packet) error
func (r *Recv) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error
func (r *Recv) Packets() <-chan *packetbuf.Packet
```

Rules:

```text
Recv does not read transport sockets.
Recv does not read TUN.
Recv does not write TUN.
Recv does not own transport writers.
Recv.WriteTo decodes protocol frames from transport payloads with their observed leg.
Recv emits received or recovered IP packets through Packets().
Recv passes HELLO, HELLO_ACK, PING, PONG, CLOSE, BW_PROBE, BW_PROBE_ACK, and LINK_STATUS to Handler.
Recv does not pass DATA or REPAIR to Handler.
Recv only accepts DATA or REPAIR for sessions admitted by the shared Session Manager.
Unknown-session DATA or REPAIR is dropped.
Recv owns per-lane receive-side FEC windows and a session-scoped emit dedupe.
Recv keeps a per-lane QoS estimator beside the receive FEC window. Recv is
only glue for QoS and FEC-health events: it extracts facts from DATA, REPAIR,
and FEC-window results, then immediately submits those facts to the estimator.
Recv must not keep QoS rate counters, FEC-health pending counters, adaptive
repair-count policy state, QoS decision-filter state, or Recv-side helpers that
calculate LINK_STATUS state. Do not add a Recv-owned `rxFECPolicy`,
`fecPolicies`, FEC-health ticker, post-tick hook, or equivalent renamed
mechanism. The estimator owns all QoS/FEC-health storage and calculation.

The estimator consumes only real receive-side sample classes and keeps them
separate:

```text
originalDataBytes = DATA bytes received without REPAIR and without late arrivals
expectedBytes     = source DATA bytes known from a completed or recovered FEC
                    group and IP packet header length parsing
repairBytes       = received REPAIR symbol bytes
lateDataBytes     = original DATA bytes that arrive after the same packet id
                    was recovered by FEC
```

A periodic estimator tick converts these pending byte counters into rate
observations and clears the pending counters for the next tick. The tick period
is one second:

```text
originalDataBps = originalDataBytes / deltaT
expectedBps     = expectedBytes / deltaT
repairBps       = repairBytes / deltaT
lateDataBps     = lateDataBytes / deltaT
```

DATA-leg QoS is judged from `expectedBytes` versus received DATA bytes.
`originalDataBytes` is accounted when an original DATA packet is accepted for
emit. `lateDataBytes` is accounted when a recovered packet's original DATA
arrives later. DATA-leg rate comparison uses `originalDataBytes +
lateDataBytes`, while `lateDataBytes` remains a separate late-arrival
observation for FEC health and logging. The estimator must not add a fifth
pending byte class for derived repair expectations. The estimator stores each
tick's `rateGap`, averages three consecutive tick gaps, and compares that
three-sample average with the QoS thresholds.

Shadow/REPAIR QoS uses the same tick cadence but a REPAIR-specific expected
rate. When a group completes, the estimator derives a group repair scale from
the largest source packet in that group and the decoded REPAIR-frame repair
count:

```text
groupRepairScale = maxSourceBytes / expectedBytes * repairCount
```

The estimator updates a direction-local EMA with this scale when the group is
submitted. The tick does not recompute group ratios; it computes:

```text
expectedRepairBps = expectedBps * repairScaleEMA
deliveryGap       = rateGapRatio(expectedRepairBps, repairBps)
loadGap           = rateGapRatio(expectedBps, repairBps)
```

The REPAIR-side limited decision uses the three-sample average of
`deliveryGap`: if REPAIR is not delivering its own expected load, the repair
transport kind is limited. The REPAIR-side clear decision requires both
three-sample averages to be below the clear threshold:

```text
deliveryGap <= 0.03
loadGap     <= 0.10
```

`deliveryGap` proves the REPAIR symbols are being delivered for the repair load
the peer actually sent. `loadGap` proves that the shadow leg is carrying a load
close to the current DATA expectation. The clear threshold allows up to 10%
measurement slack, so the shadow leg must carry at least 90% of the DATA
expectation. A low-rate REPAIR trickle must not clear a transport kind for DATA:
with the default 4+1 FEC shape, one REPAIR for four source packets carries
about 25% of `expectedBps`, so successful delivery of that default repair load
is not recovery evidence for DATA-primary capacity.
Inconsistent REPAIR-frame repair counts within one group make that group
unusable for repair-scale updates.

QoS limited/clear decisions are made only by estimator ticks. DATA and REPAIR
arrival paths submit raw byte facts to the estimator immediately; group
lifetime must not decide whether `originalDataBytes`, `repairBytes`, or
`lateDataBytes` can be accounted. `originalDataBytes` is DATA arrival state,
`repairBytes` is REPAIR arrival state, and `lateDataBytes` is late DATA arrival
state. Only `expectedBytes` is a FEC-group result: it is submitted when the
group completes or recovers and the receiver knows the source DATA byte total.
None of these paths may submit limited or clear state directly.
FEC health observations such as incomplete-group `DataArrived/DataExpected`
do not directly feed the QoS limited/clear detector. They are submitted to the
same lane-local QoS estimator as FEC-health facts and drive adaptive
repair count. That adaptive repair count has two purposes:

1. When the current DATA primary is losing heavily, QoS rate detection may lack
   enough accepted DATA/group evidence. Raising repair count lets the shadow
   REPAIR leg recover more source packets sooner, restoring tunnel throughput
   and keeping receive-side group facts flowing.
2. When the current DATA primary is backpressured and many originals arrive
   late, raising repair count lets the shadow REPAIR leg recover those packets
   before the delayed originals arrive, reducing upper-layer wait time.

The higher shadow REPAIR load created by adaptive repair count also supplies
the only valid shadow-capacity evidence for clearing a previously limited
transport kind. A clear requires the shadow leg to deliver REPAIR under load
within 10% of the current DATA expectation; FEC health itself is not a separate
QoS limited/clear signal.

FEC health has two separate inputs:

- loss health from FEC group completeness
- late health from original DATA that arrived after the same packet id was
  recovered by FEC

These inputs must stay separate. Do not merge them into a shared pressure value
or a shared EMA. The estimator updates the loss-health EMA when each group fact
is submitted:

```text
groupLossRatio = (DataExpected - DataArrived) / DataExpected
lossEMA        = EMA(lossEMA, groupLossRatio, 0.75 when rising, 0.05 when falling)
lossRepairCount = clamp(ceil(lossEMA * 4), 1, 4)
```

Loss health must react quickly to rising loss. Its upward EMA parameter and
repair-count thresholds are therefore more aggressive than late health.

The estimator updates the late-health EMA only from the estimator tick's own
pending byte counters:

```text
lateRatio   = lateDataBytes / expectedBytes
lateEMA     = EMA(lateEMA, lateRatio, 0.20 when rising, 0.05 when falling)

lateRepairCount = 1 when lateEMA <= 0.50
lateRepairCount = 2 when lateEMA <= 0.75
lateRepairCount = 3 when lateEMA <= 1.00
lateRepairCount = 4 when lateEMA >  1.00
```

If `expectedBytes` is zero for the tick, late health is not updated because the
tick has no DATA-load denominator. Late health must be slower and more
conservative than loss health, so transient late arrivals do not frequently
raise repair traffic. When late arrivals stop, late health decays through its
own EMA and may lower its own repair demand.

The shared lane-local estimator tick does not recompute group loss ratios or
consume a separate FEC-health sample queue. It commits the adaptive
`repairCount` from the already maintained health state:

```text
repairCount = max(lossRepairCount, lateRepairCount)
```

Do not add a separate ticker, loop, timer, goroutine, independently scheduled
tick, or Recv-owned flush path for FEC health.
The estimator records the current primary transport direction for the lane. The
shadow direction is the opposite transport kind and does not need separate
storage. All estimator runtime state is keyed only by DATA/REPAIR direction and
role:

```text
(dataKind, repairKind, role)
```

where role is primary/DATA or shadow/REPAIR. Rate EMAs, PID correction,
limited state, delivered-bps estimates, FEC-health EMA state, and
adaptive repair-count state must not be stored in a map keyed only by transport
kind such as UDP or TCP. UDP/TCP are transport values inside a direction and are
allowed only as facts in that direction key or as fields in the final
LINK_STATUS projection.

Primary/DATA role state is used to judge the current primary DATA leg, and
shadow/REPAIR role state is used to observe the current shadow leg. Each tick
evaluates only the state for the current role; non-current role state does not
consume the tick or emit LINK_STATUS state. The three-sample gap window is the
QoS smoothing mechanism; do not add a separate minimum group-count gate before
evaluating QoS.
Before changing the current primary direction, the estimator resets the target
primary/DATA state that would otherwise carry stale `actual` or `expected` rate
history into the new primary leg. It also resets the old primary's
shadow/REPAIR state that would otherwise carry stale `repair` history into
shadow observation. The unrelated side of each transport's role state is left
intact.
Adaptive `repairCount` is FEC-health state, not a pending sample counter. Tick
flushes may clear the FEC-health dirty flag after applying the current loss
EMA, but they must not reset `repairCount` except through the explicit reset
paths below:

- an actual primary protocol switch between UDP and TCP
- a direction-local high-repair dwell reset after computed `repairCount=4`
  persists for 75 seconds

The dwell timer starts when the computed adaptive repair count first reaches
`4`, clears when the computed count falls below `4`, and is reset by primary
protocol switches. When the dwell expires, only the direction-local FEC-health
state is reset to `repairCount=1`; QoS limited/clear state, rate windows, and
delivered-bps snapshots are preserved. Subsequent loss or late samples may raise
repair count again and start a new dwell interval. Ordinary estimator ticks,
empty ticks, clear/limited decisions that do not switch protocol, LINK_STATUS
send/drop/failure paths, and Recv glue must otherwise preserve the current
value. When a reset changes `repairCount`, the reset must happen before the
committed LINK_STATUS snapshot is emitted so the sent repair-count bits and the
estimator's local committed state are identical.
The final LINK_STATUS snapshot is a projection computed from committed
direction-local role state. It may expose UDP and TCP fields because the
protocol encodes the snapshot that way, but the estimator must not maintain a
separate transport-indexed state cache just to build that output. An aggregated
UDP/TCP snapshot must not gate another direction's QoS judgment. LINK_STATUS is
emitted as state-change feedback, not as continuous bandwidth telemetry.
`UDPDeliveredBps` and `TCPDeliveredBps` are auxiliary values carried with a
clear/limited snapshot; a delivered-bps-only change generally does not require
a new LINK_STATUS frame. The exception is the both-limited case: if updated
delivered-bps estimates change the QoS-preferred primary leg, the receiver
sends a fresh LINK_STATUS snapshot so the sender selector is not held to stale
relative bps.
Recv must not feed DATA rejected by session emit dedupe into
`originalDataBytes`, loss health, recovery, emit state, or the receive FEC
window. Recv forwards the late-DATA fact to the lane-local QoS estimator. The
estimator may account it once as `lateDataBytes` only when its recovered-packet
state has seen the packet id; `lateDataBytes` must stay separate from
`originalDataBytes`. Duplicate original DATA that was already emitted as
original DATA is not late DATA. QoS must not create synthetic DATA bytes,
mature rate samples, or bandwidth-estimation inputs from discarded DATA or
unrecovered DATA. FEC-recovered DATA may
contribute only to `expectedBytes` after parsing the recovered IP packet header
length. Unrecoverable or expired groups may only contribute to adaptive FEC
health observations such as
`DataArrived/DataExpected`; they must not feed QoS limited/clear rate state.
Receive-side QoS must not add a `lossRepair` detector or assume that a newly
sent LINK_STATUS repair count has already affected the peer sender's current
FEC groups. The current receive group uses the decoded REPAIR-frame repair
count.
Recv must not import or call concrete Send.
```

## Runtime RecvHandler

Role:

```text
control-frame glue between Recv, Session, Send.WriteFrame, and probe packages
```

Interface sketch:

```go
package runtime

type Config struct {
    BWReferenceBps uint64
    BWCapBps       uint64
    QoSWriter      *QoSWriter
}

func NewRecvHandler(s *send.Send, sessions *session.Manager, configs ...Config) *RecvHandler
```

Rules:

```text
RecvHandler implements recv.Handler.
RecvHandler constructs HELLO_ACK, PONG, BW_PROBE_ACK, and CLOSE replies and writes them through Send.WriteFrame.
RecvHandler validates HELLO_ACK by calling Session.Ack before any send-side readiness callback can run.
RecvHandler routes inbound PONG to the send-registered ping instance through LaneManager.
RecvHandler routes inbound BW_PROBE_ACK to the send-registered BwLoop through LaneManager.
RecvHandler routes inbound LINK_STATUS into the lane QoS input registered in LaneManager.
Runtime QoSWriter converts Recv QoS callbacks into outbound LINK_STATUS frames
after LINK_STATUS has been negotiated for that session and lane. QoSWriter sends
those LINK_STATUS frames through `Send.WriteFrame` with a TCP transport ref for
the target lane, not through the lane's default control-transport policy.
RecvHandler does not touch lane/leg internals directly.
RecvHandler does not handle DATA or REPAIR.
```

## Session

Role:

```text
session id, nonce, and HELLO open/ack/retry state
```

Interface sketch:

```go
package session

type Manager struct{}

func New() (*Session, error)

func (m *Manager) Get(id uint64) (*Session, bool)
func (m *Manager) Add(s *Session) bool
func (m *Manager) Create(id uint64) (*Session, bool)
func (m *Manager) GetOrCreate(id uint64) (*Session, bool)
func (m *Manager) GetOrDelete(id uint64) (*Session, bool)
func (m *Manager) Delete(id uint64)

type HelloConfig struct {
    RetryInterval time.Duration
    MaxRetries    int
    TimeoutMS     uint64
    OnAck         func()
}

type FrameSender func(ctx context.Context, v View) error

type Session struct{}

func (s *Session) Open(ctx context.Context, cfg HelloConfig, sender FrameSender, onExpire func()) *Hello
func (s *Session) Ack(nonce uint64, accepted bool) bool
func (s *Session) Do(fn func(View) error) error

type Hello struct{}

func (h *Hello) Do(fn func(View) error) error
func (h *Hello) Ack()

type View struct{}

func (v View) SessionID() uint64
func (v View) Nonce() uint64
```

Rules:

```text
New creates a local outbound Session with a fresh opaque random session id.
Manager only owns session lifetime and creation admission.
Session only owns session id, nonce, and HELLO open/ack/retry state.
Hello drives its retry loop through the caller-provided FrameSender.
Session does not know lanes, transport legs, protocol frames, FEC, schedule strategy, caps, fallback, or packet output.
Session methods do not encode protocol frames and do not write transport packets.
Callers construct HELLO/HELLO_ACK frames outside Session and send them through Send.WriteFrame.
Do not add OpenLane, RunnableLanes, ReceiveHello, ReceiveHelloAck, AcceptHello,
or other lane/protocol-specific methods to Session.
```

## Probe Packages

Role:

```text
semantic ping and bandwidth-probe state machines
```

Rules:

```text
probe/ping owns active PING timing, pending PING bookkeeping, PONG validation, RTT estimation, and liveness thresholds for one concrete lane transport path.
probe/bw owns bandwidth probe send state, received sequence bookkeeping, ACK accounting, and sample calculation.
Probe packages do not import Send or Protocol.
Probe packages do not encode frames and do not write transport packets.
Send creates active ping and bandwidth probe instances with callbacks that adapt semantic values to protocol frames via Send.WriteFrame.
Runtime RecvHandler feeds inbound PONG and BW_PROBE_ACK back into the registered instances through LaneManager.
```

## Schedule Strategy

Role:

```text
lane pick algorithm
```

Interface sketch:

```go
package schedule

type Lane interface {
    comparable
    Weight() uint32
}

type Strategy[L Lane] interface {
    Pick(lanes []L, cost uint32) (L, bool)
}
```

Rules:

```text
Schedule Strategy is not a runtime loop or packet queue owner.
Schedule Strategy does not own packet queues, lane lifecycle, fallback state, transport output, protocol frames, or FEC state.
Send provides the current runnable lane candidates and packet cost.
The default v2 strategy is DRR.
The Lane interface is a local schedule package abstraction for lane weight only; it is not a public runtime Lane module.
```

## Transport

Role:

```text
socket I/O close to Go net primitives
```

Interface sketch:

```go
type PacketWriter interface {
    WriteTo(ctx context.Context, leg LegRef, packet *packetbuf.Packet) error
}

type LegFailureHandler interface {
    OnLegFailure(ctx context.Context, leg LegRef, err error)
}

type PacketTransport interface {
    Run(ctx context.Context, writer PacketWriter) error
    WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error)
}

type StreamTransport interface {
    Run(ctx context.Context, writer PacketWriter) error
    Dial(ctx context.Context, remote string) (LegRef, error)
    Write(ctx context.Context, connID string, payload []byte) (int, error)
    Close(ctx context.Context, connID string) error
}

type Payload struct {
    Leg    LegRef
    Packet *packetbuf.Packet
}

func RunWriter(ctx context.Context, packets <-chan Payload, packet PacketTransport, stream StreamTransport) error
```

Rules:

```text
Transport does not know sessions, Schedule Strategy, FEC, Protocol, or Frame.
Transport reads complete UDP payloads or TCP length-prefixed payloads and calls PacketWriter.WriteTo with the observed transport leg.
UDP replies for a lane must use the same UDP socket that received the packet and reply to the observed remote address.
Transport reports concrete TCP leg failures through LegFailureHandler when registered.
PacketWriter.WriteTo takes ownership of packet.
Transport RunWriter consumes Payload values and releases Payload.Packet after the transport write returns.
Transport Write/WriteTo must not retain payload after returning unless it copies bytes itself.
```

## TUN I/O Adapter

Role:

```text
TUN read loop and TUN write loop
```

Interface sketch:

```go
type PacketReader interface {
    ReadPacket(ctx context.Context) (*packetbuf.Packet, error)
}

type PacketWriter interface {
    Write(ctx context.Context, packet *packetbuf.Packet) error
}

type PacketSink interface {
    WritePacket(ctx context.Context, packet []byte) (int, error)
}

func Run(ctx context.Context, reader PacketReader, writer PacketWriter) error
func RunWriter(ctx context.Context, packets <-chan *packetbuf.Packet, sink PacketSink) error
```

Rules:

```text
TUN read loop sends packets to Send.Write.
TUN write loop consumes Recv.Packets and releases each packet after writing.
TUN does not decode protocol frames and does not know Schedule Strategy or Transport.
```

## Protocol

Role:

```text
wire protocol definitions and Frame <-> bytes encoding
```

Public behavior:

```go
func Encode(frame Frame, dst []byte) ([]byte, error)
func Decode(src []byte) (Frame, error)
```

Rules:

```text
Protocol does not know runtime modules.
Protocol does not know lane health.
Protocol does not know Schedule Strategy.
Protocol behavior is only Encode and Decode.
Frame carries one concrete Body, and Frame.Type selects which body type is valid.
Do not add public per-type body helper functions.
Current v2 negotiates CapLinkStatus with FEC. LINK_STATUS is a control frame for
receive-side lane QoS snapshots; protocol only encodes and decodes it.
```

## FEC

Role:

```text
SLC shard encode/decode
```

Public behavior:

```go
func NewCodec(dataShards, repairShards int) (*Codec, error)
func (c *Codec) Encode(shards [][]byte, key uint16) error
func (c *Codec) Reconstruct(shards [][]byte, key uint16) error
```

Rules:

```text
FEC does not know packet ids, session ids, lane ids, frames, TUN, Transport, or Schedule Strategy.
FEC core erasure coding uses a maintained library; local code adapts project shard/window semantics.
```

## Dependency Rules

Allowed:

```text
runtime glue -> Send / Recv / Session / Protocol / Transport leg types / probe packages
Send -> Protocol / FEC / Schedule Strategy / Session / Transport leg types / probe packages
Recv -> Protocol / FEC / Session / Transport leg types
Transport -> packetbuf / net primitives
TUN read loop -> Send.Write
TUN write loop -> Recv packet channel / TUN device
```

Forbidden:

```text
public Tunnel module
shared runtime-data module
public Path/Lane module interface
Session -> Protocol / Transport / FEC / Schedule Strategy / lane data / caps
Schedule Strategy -> Protocol / Transport / session data / lane runtime data
Transport -> Protocol / Schedule Strategy / FEC / session data
Recv -> TUN writer / Transport writers / concrete Send package
Send -> Recv
```
