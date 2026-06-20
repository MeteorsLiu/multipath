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
Recv keeps a per-lane QoS estimator beside the receive FEC window. The
estimator consumes complete or recovered DATA/REPAIR group samples, compares
DATA-leg delivery with FEC-derived expected delivery, estimates shadow-leg
equivalent rate from REPAIR bytes, and reports abnormal status through Recv's
QoS callback.
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
after LINK_STATUS has been negotiated for that session and lane.
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
