package transport

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

type Kind uint8

const (
	KindUDP Kind = iota + 1
	KindTCP
)

type LegRef struct {
	Kind Kind

	EndpointID string
	RemoteAddr net.Addr

	ConnID string
}

var ErrInvalidLeg = errors.New("transport: invalid leg")

type Payload struct {
	Leg    LegRef
	Packet *packetbuf.Packet
}

const legWriterQueueSize = 128

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

func RunWriter(ctx context.Context, packets <-chan Payload, packet PacketTransport, stream StreamTransport) error {
	dispatcher := newLegWriterDispatcher(ctx, packet, stream)
	defer dispatcher.close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case payload, ok := <-packets:
			if !ok {
				return nil
			}
			if payload.Packet == nil {
				continue
			}
			if err := dispatcher.dispatch(payload); err != nil {
				return err
			}
		case err := <-dispatcher.errs:
			return err
		}
	}
}

type legWriterDispatcher struct {
	ctx    context.Context
	packet PacketTransport
	stream StreamTransport

	mu      sync.Mutex
	writers map[writerKey]chan Payload
	errs    chan error
	wg      sync.WaitGroup
}

type writerKey struct {
	kind       Kind
	endpointID string
	remote     string
	connID     string
}

func newLegWriterDispatcher(ctx context.Context, packet PacketTransport, stream StreamTransport) *legWriterDispatcher {
	return &legWriterDispatcher{
		ctx:     ctx,
		packet:  packet,
		stream:  stream,
		writers: make(map[writerKey]chan Payload),
		errs:    make(chan error, 1),
	}
}

func (d *legWriterDispatcher) dispatch(payload Payload) error {
	key, ok := payloadWriterKey(payload.Leg)
	if !ok {
		payload.Packet.Release()
		return ErrInvalidLeg
	}

	d.mu.Lock()
	ch := d.writers[key]
	if ch == nil {
		ch = make(chan Payload, legWriterQueueSize)
		d.writers[key] = ch
		d.wg.Add(1)
		go d.runLegWriter(key, ch)
	}
	d.mu.Unlock()

	select {
	case ch <- payload:
		return nil
	case <-d.ctx.Done():
		payload.Packet.Release()
		return d.ctx.Err()
	default:
		debuglog.Printf("transport", "writer drop queue_full %s bytes=%d", debugLeg(payload.Leg), len(payload.Packet.Payload))
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", kindLabel(payload.Leg.Kind)),
			metrics.L("operation", "write_queue_full"),
		)
		payload.Packet.Release()
		return nil
	}
}

func (d *legWriterDispatcher) runLegWriter(key writerKey, ch <-chan Payload) {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			d.releaseQueued(ch)
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if debuglog.Enabled() {
				debuglog.Printf("transport", "writer dispatch %s bytes=%d", debugLeg(payload.Leg), len(payload.Packet.Payload))
			}
			err := writePayload(d.ctx, payload, d.packet, d.stream)
			payload.Packet.Release()
			if err != nil {
				debuglog.Printf("transport", "writer error %s err=%v", debugLeg(payload.Leg), err)
				metrics.IncCounter(metrics.TransportErrorsTotal,
					metrics.L("transport", kindLabel(payload.Leg.Kind)),
					metrics.L("operation", "write_dispatch"),
				)
				d.releaseQueued(ch)
				select {
				case d.errs <- err:
				case <-d.ctx.Done():
				default:
				}
				return
			}
		}
	}
}

func (d *legWriterDispatcher) releaseQueued(ch <-chan Payload) {
	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if payload.Packet != nil {
				payload.Packet.Release()
			}
		default:
			return
		}
	}
}

func (d *legWriterDispatcher) close() {
	d.mu.Lock()
	tcpConnIDs := make([]string, 0, len(d.writers))
	for key := range d.writers {
		if key.kind == KindTCP && key.connID != "" {
			tcpConnIDs = append(tcpConnIDs, key.connID)
		}
	}
	for _, ch := range d.writers {
		close(ch)
	}
	d.writers = nil
	d.mu.Unlock()
	if d.stream != nil {
		for _, connID := range tcpConnIDs {
			_ = d.stream.Close(context.Background(), connID)
		}
	}
	d.wg.Wait()
}

func payloadWriterKey(leg LegRef) (writerKey, bool) {
	key := writerKey{kind: leg.Kind}
	switch leg.Kind {
	case KindUDP:
		if leg.EndpointID == "" || leg.RemoteAddr == nil {
			return writerKey{}, false
		}
		key.endpointID = leg.EndpointID
		key.remote = leg.RemoteAddr.String()
		return key, true
	case KindTCP:
		if leg.ConnID == "" {
			return writerKey{}, false
		}
		key.connID = leg.ConnID
		return key, true
	default:
		return writerKey{}, false
	}
}

func kindLabel(kind Kind) string {
	switch kind {
	case KindUDP:
		return "udp"
	case KindTCP:
		return "tcp"
	default:
		return "unknown"
	}
}

func writePayload(ctx context.Context, payload Payload, packet PacketTransport, stream StreamTransport) error {
	switch payload.Leg.Kind {
	case KindUDP:
		if packet == nil || payload.Leg.EndpointID == "" || payload.Leg.RemoteAddr == nil {
			return ErrInvalidLeg
		}
		_, err := packet.WriteTo(ctx, payload.Leg.EndpointID, payload.Leg.RemoteAddr, payload.Packet.Payload)
		return err
	case KindTCP:
		if stream == nil || payload.Leg.ConnID == "" {
			return ErrInvalidLeg
		}
		_, err := stream.Write(ctx, payload.Leg.ConnID, payload.Packet.Payload)
		if err != nil && !errors.Is(err, context.Canceled) {
			if debuglog.Enabled() {
				debuglog.Printf("transport", "drop stale tcp payload conn=%s bytes=%d err=%v", payload.Leg.ConnID, len(payload.Packet.Payload), err)
			}
			return nil
		}
		return err
	default:
		return ErrInvalidLeg
	}
}
