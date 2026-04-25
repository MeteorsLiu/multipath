# Multipath Tunnel Architecture

This document describes the runtime module boundaries for the multipath tunnel
refactor. The wire format is described in [protocol.md](protocol.md).

## Module Boundaries

Public runtime modules:

```text
Send
Recv
ProbeLoop
Scheduler
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
Session
Lane
Path
runtime state
```

Session and lane data are internal runtime data. TUN is an I/O adapter. Runtime
glue does not decode frames, choose lanes, or mutate session/lane data.

## Construction

The application wires modules explicitly:

```go
probeEvents := make(chan core.Event, 128)

sender := send.New(send.Config{
    StreamTransport: streamTransport,
    ProbeInterval:   probeInterval,
    ProbeTimeout:    probeTimeout,
    ProbeEvents:     probeEvents,
    BootstrapLanes:  bootstrap,
})

probeLoop := probe.New(sender, probe.Config{
    Events:   probeEvents,
    Interval: probeInterval,
    Timeout:  probeTimeout,
})

receiver := recv.New(recv.Config{
    Controller: probeLoop,
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
```

## Data Flow

TUN packet to transport:

```text
TUN
 -> tun.Run
 -> Send.Write(packet)
 -> DATA frame
 -> optional REPAIR frame
 -> Scheduler.Dequeue()
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
 -> ProbeLoop.Write(frame, leg)
 -> Send internal lane/session state
 -> optional Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

Probe loop:

```text
probe timer / HELLO retry / fallback dial result
 -> ProbeLoop.Run
 -> Send internal lane/session state
 -> optional Send.Packets()
 -> transport.RunWriter
 -> UDP/TCP Transport
```

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
per-session Scheduler
HELLO bootstrap and retry data
transport-bound output queue
```

Interface sketch:

```go
package send

type Config struct {
    StreamTransport transport.StreamTransport
    ProbeInterval   time.Duration
    ProbeTimeout    time.Duration
    ProbeEvents     chan core.Event
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

type Result struct {
    Accepted   bool
    Caps       uint16
    FECProfile uint8
}

func New(configs ...Config) *Send
func (s *Send) Write(ctx context.Context, packet *packetbuf.Packet) error
func (s *Send) WriteTo(ctx context.Context, leg transport.LegRef, packet *packetbuf.Packet) error
func (s *Send) Packets() <-chan transport.Payload

func (s *Send) Bootstrap(ctx context.Context) error
func (s *Send) WriteFrame(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (Result, error)
func (s *Send) WriteProbeEvent(ctx context.Context, event core.Event) error
func (s *Send) RetryHELLO(ctx context.Context, nowMS uint64) error
```

Rules:

```text
Send does not read TUN.
Send does not read transport sockets.
Send owns Scheduler.Enqueue/Dequeue.
Send owns DATA and REPAIR creation.
Send is the only module that schedules TUN packets across lanes.
Send.Write takes ownership of the TUN packet and releases it before returning.
Send.WriteTo takes ownership of an already encoded transport payload and emits
it through Send.Packets().
Send.Packets returns transport-bound packets. The consumer releases each packet
after the transport write returns.
Send exposes only the narrow maintenance entry points needed by ProbeLoop:
bootstrap, decoded control frame input, probe-core event input, and HELLO retry
tick input.
Send does not expose semantic control methods such as AcceptHello,
AcceptHelloAck, ObserveLane, ReceivePing, ReceivePong, or Close.
```

## Recv

Role:

```text
transport payload ingress and receive-side protocol handling
```

Interface sketch:

```go
package recv

type Config struct {
    Controller interface {
        Write(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (Result, error)
    }
}

type Result struct {
    Accepted   bool
    Caps       uint16
    FECProfile uint8
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
Recv owns receive-side FEC windows.
Recv writes decoded control frames to its configured ProbeLoop writer.
Recv must not call Send directly.
```

## ProbeLoop

Role:

```text
control-plane and probe-loop adapter for Send
```

Interface sketch:

```go
package probe

type Config struct {
    Events   <-chan core.Event
    Interval time.Duration
    Timeout  time.Duration
}

func New(s *send.Send, configs ...Config) *Loop
func (l *Loop) Bootstrap(ctx context.Context) error
func (l *Loop) Write(ctx context.Context, frame protocol.Frame, leg transport.LegRef) (recv.Result, error)
func (l *Loop) Run(ctx context.Context) error
```

Rules:

```text
ProbeLoop is the only non-TUN control-plane adapter into Send.
Recv writes decoded protocol frames to ProbeLoop, not to Send.
Runtime bootstrap calls ProbeLoop.Bootstrap.
ProbeLoop.Run drives probe, HELLO retry, and fallback flow.
ProbeLoop does not read TUN or transport sockets.
ProbeLoop does not schedule DATA directly.
ProbeLoop is implemented in the probe package. It depends on Send only through
Send's narrow maintenance entry points.
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
Transport does not know sessions, lanes, Scheduler, FEC, Protocol, or Frame.
Transport reads complete UDP payloads or TCP length-prefixed payloads and calls
PacketWriter.WriteTo with the observed transport leg.
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
TUN does not decode protocol frames and does not know Scheduler or Transport.
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
Protocol does not know Scheduler.
Protocol behavior is only Encode and Decode.
```

## FEC

Role:

```text
4+1 SLC shard encode/decode
```

Public behavior:

```go
func NewCodec(dataShards, repairShards int) (*Codec, error)
func (c *Codec) Encode(shards [][]byte, key uint16) error
func (c *Codec) Reconstruct(shards [][]byte, key uint16) error
```

Rules:

```text
FEC does not know packet ids, session ids, lane ids, frames, TUN, Transport, or Scheduler.
```

## Dependency Rules

Allowed:

```text
runtime glue -> Send / Recv / Transport / TUN
runtime glue -> ProbeLoop
Send -> Protocol / FEC / Scheduler / Transport leg types / Probe core event type
Recv -> Protocol / FEC / Transport leg types / configured ProbeLoop writer
ProbeLoop -> Send maintenance entry points / Protocol / Transport leg types / Recv result type / Probe core runner
Transport -> packetbuf / net primitives
TUN write loop -> Recv packet channel / TUN device
```

Forbidden:

```text
public Tunnel module
shared runtime-data module
public Session module or Session interface
public Path/Lane module interface
Scheduler -> Protocol / Transport / session data
Transport -> Protocol / Scheduler / FEC / session data
Recv -> TUN writer / Transport writers
Recv -> concrete Send package
Send -> Recv
```
