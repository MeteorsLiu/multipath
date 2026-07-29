package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

func TestRunWriterReleasesPacketOnInvalidLeg(t *testing.T) {
	packet := packetbuf.Acquire(16)
	packets := make(chan Payload, 1)
	packets <- Payload{Packet: packet}
	close(packets)

	err := RunWriter(context.Background(), packets, nil, nil)
	if !errors.Is(err, ErrInvalidLeg) {
		t.Fatalf("RunWriter err = %v, want ErrInvalidLeg", err)
	}
	if packet.Payload != nil {
		t.Fatal("packet was not released after write error")
	}
}

func TestRunWriterDropsStaleTCPConn(t *testing.T) {
	for name, writeErr := range map[string]error{
		"unknown": ErrUnknownConn,
		"closed":  net.ErrClosed,
	} {
		t.Run(name, func(t *testing.T) {
			packet := packetbuf.Acquire(16)
			packets := make(chan Payload, 1)
			packets <- Payload{
				Leg:    LegRef{Kind: KindTCP, ConnID: "stale"},
				Packet: packet,
			}
			close(packets)

			stream := &runWriterStream{err: writeErr}
			if err := RunWriter(context.Background(), packets, nil, stream); err != nil {
				t.Fatalf("RunWriter err = %v, want nil", err)
			}
			if stream.writes != 1 {
				t.Fatalf("stream writes = %d, want 1", stream.writes)
			}
			if packet.Payload != nil {
				t.Fatal("packet was not released after stale TCP write")
			}
		})
	}
}

func TestRunWriterDropsTransientUDPWriteError(t *testing.T) {
	packet := packetbuf.Acquire(16)
	packets := make(chan Payload, 1)
	packets <- Payload{
		Leg: LegRef{
			Kind:       KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		},
		Packet: packet,
	}
	close(packets)

	writer := &runWriterPacket{err: syscall.ENETUNREACH}
	if err := RunWriter(context.Background(), packets, writer, nil); err != nil {
		t.Fatalf("RunWriter err = %v, want nil", err)
	}
	if writer.writes != 1 {
		t.Fatalf("packet writes = %d, want 1", writer.writes)
	}
	if packet.Payload != nil {
		t.Fatal("packet was not released after UDP write error")
	}
}

func TestLegWriterQueueSizes(t *testing.T) {
	for _, kind := range []Kind{KindUDP, KindTCP} {
		if got := legWriterQueueSize(kind); got != 1024 {
			t.Fatalf("queue size for kind %d = %d, want 1024", kind, got)
		}
	}
}

func TestRunWriterDoesNotBlockUDPBehindBlockedTCP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := make(chan Payload, 2)
	stream := &runWriterStream{
		block:   make(chan struct{}),
		started: make(chan struct{}),
	}
	packet := &runWriterPacket{wrote: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWriter(ctx, packets, packet, stream)
	}()

	tcpPacket := packetbuf.Acquire(16)
	udpPacket := packetbuf.Acquire(16)
	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: tcpPacket,
	}
	packets <- Payload{
		Leg: LegRef{
			Kind:       KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		},
		Packet: udpPacket,
	}

	select {
	case <-packet.wrote:
	case <-time.After(time.Second):
		t.Fatal("UDP write blocked behind TCP write")
	}

	cancel()
	close(stream.block)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWriter err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWriter did not stop after context cancellation")
	}
}

func TestUDPWriterBatchesQueuedPayloads(t *testing.T) {
	ctx := context.Background()
	packet := &runWriterBatchPacket{batch: 4}
	dispatcher := &legWriterDispatcher{
		ctx:    ctx,
		packet: packet,
		errs:   make(chan error, 1),
	}
	key := writerKey{kind: KindUDP, endpointID: "udp0", remote: "127.0.0.1:1234"}
	remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	ch := make(chan Payload, 3)
	payloads := make([]Payload, 3)
	for i := range payloads {
		pkt := packetbuf.Acquire(16)
		pkt.Payload[0] = byte(i)
		pkt.SetLen(1)
		payloads[i] = Payload{
			Leg: LegRef{
				Kind:       KindUDP,
				EndpointID: "udp0",
				RemoteAddr: remote,
			},
			Packet: pkt,
		}
		ch <- payloads[i]
	}
	close(ch)

	dispatcher.wg.Add(1)
	dispatcher.runUDPWriter(key, ch)

	if packet.batchCalls != 1 {
		t.Fatalf("batch calls = %d, want 1", packet.batchCalls)
	}
	if got := packet.batchLens[0]; got != len(payloads) {
		t.Fatalf("batch len = %d, want %d", got, len(payloads))
	}
	if packet.writes != 0 {
		t.Fatalf("fallback writes = %d, want 0", packet.writes)
	}
	for i, payload := range payloads {
		if payload.Packet.Payload != nil {
			t.Fatalf("payload %d was not released", i)
		}
	}
}

func TestTCPWriterBatchesQueuedPayloads(t *testing.T) {
	ctx := context.Background()
	stream := &runWriterBatchStream{}
	dispatcher := &legWriterDispatcher{
		ctx:    ctx,
		stream: stream,
		errs:   make(chan error, 1),
	}
	key := writerKey{kind: KindTCP, connID: "tcp0"}
	ch := make(chan Payload, 3)
	payloads := make([]Payload, 3)
	for i := range payloads {
		pkt := packetbuf.Acquire(16)
		pkt.Payload[0] = byte(i)
		pkt.SetLen(1)
		payloads[i] = Payload{
			Leg:    LegRef{Kind: KindTCP, ConnID: "tcp0"},
			Packet: pkt,
		}
		ch <- payloads[i]
	}
	close(ch)

	dispatcher.wg.Add(1)
	dispatcher.runTCPWriter(key, ch)

	if stream.batchCalls != 1 {
		t.Fatalf("batch calls = %d, want 1", stream.batchCalls)
	}
	if got := stream.batchLens[0]; got != len(payloads) {
		t.Fatalf("batch len = %d, want %d", got, len(payloads))
	}
	if stream.writes != 0 {
		t.Fatalf("fallback writes = %d, want 0", stream.writes)
	}
	for i, payload := range payloads {
		if payload.Packet.Payload != nil {
			t.Fatalf("payload %d was not released", i)
		}
	}
}

func TestRunWriterDropsWhenLegQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := make(chan Payload, tcpLegWriterQueueSize+1)
	stream := &runWriterStream{
		block:   make(chan struct{}),
		started: make(chan struct{}),
	}
	packet := &runWriterPacket{wrote: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWriter(ctx, packets, packet, stream)
	}()

	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: packetbuf.Acquire(16),
	}

	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("blocked writer did not receive first packet")
	}

	for i := 0; i < tcpLegWriterQueueSize; i++ {
		packets <- Payload{
			Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
			Packet: packetbuf.Acquire(16),
		}
	}

	deadline := time.After(time.Second)
	for len(packets) > 0 {
		select {
		case <-deadline:
			t.Fatalf("RunWriter did not drain initial packets, remaining=%d", len(packets))
		default:
			time.Sleep(time.Millisecond)
		}
	}

	droppedPacket := packetbuf.Acquire(16)
	packets <- Payload{
		Leg:    LegRef{Kind: KindTCP, ConnID: "blocked"},
		Packet: droppedPacket,
	}
	packets <- Payload{
		Leg: LegRef{
			Kind:       KindUDP,
			EndpointID: "udp0",
			RemoteAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		},
		Packet: packetbuf.Acquire(16),
	}

	select {
	case <-packet.wrote:
	case <-time.After(time.Second):
		t.Fatal("UDP write blocked behind full TCP queue")
	}
	if droppedPacket.Payload != nil {
		t.Fatal("packet was not released after full queue drop")
	}
	select {
	case err := <-errCh:
		t.Fatalf("RunWriter exited after full queue drop: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	cancel()
	close(stream.block)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunWriter err = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWriter did not stop after context cancellation")
	}
}

type runWriterStream struct {
	err     error
	block   chan struct{}
	started chan struct{}
	once    sync.Once
	writes  int
}

func (s *runWriterStream) Run(ctx context.Context, writer PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *runWriterStream) Dial(ctx context.Context, remote string) (LegRef, error) {
	return LegRef{}, nil
}

func (s *runWriterStream) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	s.writes++
	s.once.Do(func() {
		if s.started != nil {
			close(s.started)
		}
	})
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, s.err
}

func (s *runWriterStream) Close(ctx context.Context, connID string) error {
	return nil
}

type runWriterPacket struct {
	once   sync.Once
	wrote  chan struct{}
	err    error
	writes int
}

func (p *runWriterPacket) Run(ctx context.Context, writer PacketWriter) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *runWriterPacket) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	p.writes++
	p.once.Do(func() {
		if p.wrote != nil {
			close(p.wrote)
		}
	})
	return len(payload), p.err
}

type runWriterBatchPacket struct {
	runWriterPacket
	batch      int
	batchCalls int
	batchLens  []int
}

func (p *runWriterBatchPacket) batchSize(endpointID string) int {
	return p.batch
}

func (p *runWriterBatchPacket) writeBatchTo(ctx context.Context, endpointID string, remote net.Addr, payloads []Payload) (int, error) {
	p.batchCalls++
	p.batchLens = append(p.batchLens, len(payloads))
	return len(payloads), nil
}

type runWriterBatchStream struct {
	runWriterStream
	batchCalls int
	batchLens  []int
}

func (s *runWriterBatchStream) writePayloadBatch(ctx context.Context, connID string, payloads []Payload) error {
	s.batchCalls++
	s.batchLens = append(s.batchLens, len(payloads))
	releasePayloads(payloads)
	return nil
}
