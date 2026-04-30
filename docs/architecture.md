# Multipath Tunnel Architecture

This document describes the runtime module boundaries for the multipath tunnel
refactor. The wire format is described in [protocol.md](protocol.md).

## Module Boundaries

Public runtime modules:

```text
Send
Recv
ProbeLoop
Session
Schedule Strategy
Transport
Protocol
FEC
TUN I/O adapter
```

There is no public Tunnel module, tunnel facade, or shared runtime-data module.
Send and Recv are separate modules. Runtime glue constructs modules explicitly,
starts loops, and waits for cancellation or errors.

Not public module boundaries:

```text
Lane
Path
runtime state
```

Lane data is internal runtime data. Session lifecycle and HELLO state are owned
by the Session module. TUN is an I/O adapter. Runtime glue does not decode
frames, choose lanes, or mutate lane data.

## Construction

The application wires modules explicitly:

```go
probeEvents := make(chan core.Event, 128)
sessions := &session.Manager{}

sender := send.New(send.Config{
    StreamTransport: streamTransport,
    SessionManager:   sessions,
    ProbeInterval:   probeInterval,
    ProbeTimeout:    probeTimeout,
    ProbeEvents:     probeEvents,
    BootstrapLanes:  bootstrap,
})

probeLoop := send.NewProbeLoop(sender, send.ProbeLoopConfig{
    Events:   probeEvents,
    Interval: probeInterval,
    Timeout:  probeTimeout,
})

receiver := recv.New(recv.Config{
    Control:        send.NewRecvState(sender),
    SessionManager: sessions,
})
```

The runtime glue then starts:

```text
probeLoop.Bootstrap(ctx)
tun.Run(ctx, tunReader, sender)
transport.RunWriter(ctx, sender.Packets(), packetTransport, streamTransport)
packetTransport.Run(ctx, receiver)
streamTransport.Run(ctx, receiver)
tun.RunWriter(ctx, receiver.Packets(), tunWriter)
probeLoop.Run(ctx)
metricsServer.Run(ctx)
```

## Data Flow

TUN packet to transport:

```text
TUN
 -> tun.Run
 -> Send.Write(packet)
 -> DATA frame
 -> optional REPAIR frame
 -> Schedule Strategy Pick()
 -> Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Transport payload to TUN:

```text
UDP/TCP Transport
 -> Recv.WriteTo(leg, packet)
 -> Protocol.Decode
 -> Recv.Packets()
 -> tun.RunWriter
 -> TUN
```

Transport control payload:

```text
UDP/TCP Transport
 -> Recv.WriteTo(leg, packet)
 -> Protocol.Decode
 -> control-plane glue
 -> Session Manager / Session / Hello
 -> caller-owned protocol frame construction
 -> optional Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Recv handles control-frame classification and must not expose a control-frame
channel. DATA/REPAIR are not gated by control accept results. HELLO and
HELLO_ACK state transitions go through Session; protocol frame construction
stays in the caller and does not move into Session.

Probe loop:

```text
probe timer / HELLO retry / fallback dial result
 -> ProbeLoop.Run
 -> Send internal lane/session state
 -> optional Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Metrics:

```text
runtime
 -> metrics server
 -> /metrics

TUN / Transport / Send / Recv / ProbeLoop
 -> internal metrics counters and gauges
```

Metrics are observational only. The metrics package does not own session, lane,
protocol, FEC, schedule strategy, TUN, or transport state, and instrumentation
must not change scheduling, recovery, fallback, or packet ownership behavior.

## Send

Role:

```text
TUN packet ingress, send-side lane scheduling, and transport-bound packet queue
```

Owns:

```text
active outbound session id
send-side session data
lane runtime data
per-session Schedule Strategy
global FEC profile and codec
HELLO bootstrap and retry data
per-route HELLO timeout policy
per-leg RTT estimator sampled from PONG
RTT-driven FEC flush timer for variable-span SLC
transport-bound output queue
```

Interface sketch:

```go
package send

type Config struct {
    StreamTransport transport.StreamTransport
    SessionManager   *session.Manager
    ProbeInterval   time.Duration
    ProbeTimeout    time.Duration
    ProbeEvents     chan core.Event
    EnableFEC       bool
    FECFlushAlpha       uint32
    FECFlushMinMs       uint32
    FECFlushMaxMs       uint32
    FECFlushColdStartMs uint32
    FECFlushFixedMs     uint32
    BootstrapLanes  []BootstrapLane
}

type BootstrapLane struct {
    LaneID    uint8
    Weight    uint32
    Leg       transport.LegRef
    TCPRemote string
}

func New(configs ...Config) *Send
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error
func (s *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error
func (s *Send) Packets() <-chan transport.Payload
```

Rules:

```text
Send does not read TUN.
Send does not read transport sockets.
Send owns DATA and REPAIR scheduling through a per-session Schedule Strategy.
Send owns DATA and REPAIR creation.
Send is the only module that schedules TUN packets across lanes.
Send.Write takes ownership of the TUN packet and releases it before returning.
Send.WriteTo takes ownership of an already encoded transport payload and emits
it through Send.Packets().
Send.Packets returns transport-bound packets. The consumer releases each packet
after the transport write returns.
Send does not expose control-plane maintenance methods on `Send` itself. The
RecvState adapter lives in the send package so it can implement
`recv.ControlState` using Send-owned lane/session/probe state without making
those hooks methods on `Send`.
Send does not expose semantic control methods such as AcceptHello,
AcceptHelloAck, ObserveLane, ReceivePing, ReceivePong, or Close.
Send arms the FEC flush timer only when the negotiated FEC profile supports
variable-span REPAIR frames. The timer emits transport-bound REPAIR frames
through the same scheduling and packet queue path as fill-triggered REPAIR.
```

## Recv

Role:

```text
transport payload ingress and receive-side protocol handling
```

Interface sketch:

```go
package recv

type ControlState interface {
    OnHello(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnHelloAck(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnPing(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnPong(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
    OnClose(ctx context.Context, leg transport.LegRef, frame protocol.Frame) error
}

type Config struct {
    Control        ControlState
    SessionManager *session.Manager
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
Recv does not write transport sockets.
Recv.Write is a convenience for packet input when no transport leg is known.
Recv.WriteTo decodes protocol frames from transport payloads with their leg.
Recv emits received or recovered IP packets through Packets().
Recv passes decoded HELLO, HELLO_ACK, PING, PONG, and CLOSE frames to the
configured ControlState.
Recv does not pass DATA or REPAIR to ControlState.
Recv only accepts DATA or REPAIR for sessions admitted by the shared Session
Manager. Unknown-session DATA or REPAIR is dropped.
Recv owns receive-side FEC windows.
Recv does not own or call a control-plane writer.
Recv must not call ProbeLoop or Send directly.
Recv must not expose Result, Respond, or accept-gating plumbing.
Recv uses profile-aware FEC codecs and keeps per-session receive windows because
packet_id and base_packet_id are session-scoped. REPAIR `source_span` is
interpreted in Recv according to the negotiated FEC profile; Session does not
own that state.
Those FEC windows stay in Recv; Session does not own FEC state.
```

## Session

Role:

```text
session lifecycle admission and HELLO open/ack/retry state
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
func (m *Manager) Delete(id uint64)

type Session struct{}

func (s *Session) Open(nowMS uint64) *Hello
func (s *Session) Ack(nonce uint64, accepted bool) bool
func (s *Session) Do(fn func(View) error) error

type Hello struct{}

func (h *Hello) Do(fn func(View) error) error
func (h *Hello) Retry(nowMS uint64, fn func(View) error) (sent bool, expired bool, err error)

type View struct{}

func (v View) SessionID() uint64
func (v View) Nonce() uint64
```

Rules:

```text
New creates a local outbound Session with a fresh opaque random session id.
Manager only owns session lifetime and creation admission.
Manager does not know protocol frames, lanes, legs, transport, FEC, schedule
strategy, caps, or fallback.
Session only owns session id, nonce, and HELLO open/ack/retry state.
Session does not know lanes, transport legs, protocol frames, FEC, schedule
strategy, caps, or fallback.
Hello is the retry-capable HELLO send handle returned by Session.Open.
View has no exported fields. Callers may only read SessionID and Nonce through
methods while inside Do/Retry callbacks.
Session methods do not encode protocol frames and do not write transport
packets. Callers construct HELLO/HELLO_ACK frames inside Do/Retry callbacks and
write them through the runtime's transport-bound output path.
Do not add OpenLane, RunnableLanes, ReceiveHello, ReceiveHelloAck, AcceptHello,
or other lane/protocol-specific methods to Session.
```

Passive HELLO handling:

```go
sess, ok := sessions.GetOrCreate(frame.SessionID)
if !ok {
    // creation denied; caller may write accepted=0 or drop according to policy
}

err := sess.Do(func(v session.View) error {
    return out.WriteFrame(ctx, leg, protocol.Frame{
        Type:      protocol.TypeHELLOACK,
        SessionID: v.SessionID(),
        LaneID:    frame.LaneID,
        Body: protocol.HelloAckBody{
            Nonce:      hello.Nonce,
            Accepted:   boolByte(accepted),
            Caps:       negotiatedCaps,
            FECProfile: negotiatedFEC,
        },
    })
})
```

Active HELLO handling:

```go
hello := sess.Open(nowMS)

err := hello.Do(func(v session.View) error {
    return out.WriteFrame(ctx, leg, protocol.Frame{
        Type:      protocol.TypeHELLO,
        SessionID: v.SessionID(),
        LaneID:    laneID,
        Body: protocol.HelloBody{
            Nonce:      v.Nonce(),
            Caps:       caps,
            FECProfile: fecProfile,
        },
    })
})
```

HELLO_ACK handling:

```go
if !sess.Ack(body.Nonce, body.Accepted == 1) {
    return nil
}

// Caller updates lane readiness, negotiated caps, global FEC profile, and probe state.
```

## ProbeLoop

Role:

```text
probe-loop adapter for Send
```

Interface sketch:

```go
package send

type ProbeLoopConfig struct {
    Events   <-chan core.Event
    Interval time.Duration
    Timeout  time.Duration
}

func NewProbeLoop(s *Send, configs ...ProbeLoopConfig) *ProbeLoop
func (l *ProbeLoop) Bootstrap(ctx context.Context) error
func (l *ProbeLoop) Run(ctx context.Context) error
```

Rules:

```text
Runtime bootstrap calls ProbeLoop.Bootstrap.
ProbeLoop.Run drives probe, HELLO retry, and fallback flow.
ProbeLoop does not read TUN or transport sockets.
ProbeLoop does not schedule DATA directly.
ProbeLoop is implemented in the send package because it adapts generic
probe/core events and retry ticks to Send-owned lane state. The probe/core
package remains independent and owns only the generic probe state machine.
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
Schedule Strategy does not own packet queues, lane lifecycle, fallback state,
transport output, protocol frames, or FEC state.
Send provides the current runnable lane candidates and packet cost.
Schedule Strategy picks one candidate lane and updates its own fairness
bookkeeping as part of Pick.
The Lane interface is a local schedule package abstraction for lane weight only;
it is not a public runtime Lane module.
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
Transport reads complete UDP payloads or TCP length-prefixed payloads and calls
PacketWriter.WriteTo with the observed transport leg.
Transport reports concrete transport-leg failures through LegFailureHandler
when one is registered. The failure event carries only LegRef and error; Send
maps the leg to lane state through its existing probe target bindings.
PacketWriter.WriteTo takes ownership of packet. It must release packet before
returning or transfer ownership to its own output channel before returning.
After WriteTo returns, Transport must not read or release packet.
Transport RunWriter consumes Payload values and releases Payload.Packet after
the transport write returns.
Transport Write/WriteTo must not retain payload after returning unless it
copies bytes itself.
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
```

## Dependency Rules

Allowed:

```text
runtime glue -> Send / Recv / Transport / TUN
runtime glue -> ProbeLoop
Send -> Protocol / FEC / Schedule Strategy / Session / Transport leg types / Probe core event type
Recv -> Protocol / FEC / Session / Transport leg types
ProbeLoop -> Send maintenance entry points / Protocol / Transport leg types / Probe core runner
Transport -> packetbuf / net primitives
TUN write loop -> Recv packet channel / TUN device
```

Forbidden:

```text
public Tunnel module
shared runtime-data module
public Path/Lane module interface
Session -> Protocol / Transport / FEC / Schedule Strategy / lane data
Schedule Strategy -> Protocol / Transport / session data
Transport -> Protocol / Schedule Strategy / FEC / session data
Recv -> TUN writer / Transport writers
Recv -> concrete Send package
Send -> Recv
```
