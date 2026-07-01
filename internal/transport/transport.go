package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

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

const (
	tcpLegWriterQueueSize  = 64*1024*1024/1500 + 1
	udpLegWriterQueueSize  = 1024
	tcpPayloadBatchSize    = 128
	debugQueueWaitLogAfter = time.Millisecond
)

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

type packetBatchTransport interface {
	writeBatchTo(ctx context.Context, endpointID string, remote net.Addr, payloads []Payload) (int, error)
	batchSize(endpointID string) int
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
		ch = make(chan Payload, legWriterQueueSize(key.kind))
		d.writers[key] = ch
		switch key.kind {
		case KindUDP:
			d.wg.Add(1)
			go d.runUDPWriter(key, ch)
		case KindTCP:
			d.wg.Add(1)
			go d.runTCPWriter(key, ch)
		}
	}
	d.mu.Unlock()

	packetBytes := len(payload.Packet.Payload)
	var start time.Time
	if debuglog.Enabled() {
		start = time.Now()
	}
	select {
	case ch <- payload:
		if !start.IsZero() {
			wait := time.Since(start)
			if wait >= debugQueueWaitLogAfter {
				debuglog.Printf("transport", "leg_queue_wait leg={%s} wait_us=%d queue_len=%d queue_cap=%d bytes=%d",
					debugLeg(payload.Leg), wait.Microseconds(), len(ch), cap(ch), packetBytes)
			}
		}
		return nil
	case <-d.ctx.Done():
		payload.Packet.Release()
		return d.ctx.Err()
	}
}

func legWriterQueueSize(kind Kind) int {
	switch kind {
	case KindUDP:
		return udpLegWriterQueueSize
	case KindTCP:
		return tcpLegWriterQueueSize
	default:
		return 1
	}
}

func (d *legWriterDispatcher) runUDPWriter(key writerKey, ch <-chan Payload) {
	defer d.wg.Done()
	batchSize := udpPayloadBatchSize(d.packet, key.endpointID)
	batch := make([]Payload, 0, batchSize)
	for {
		select {
		case <-d.ctx.Done():
			d.releaseQueued(ch)
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			batch = append(batch[:0], payload)
		drain:
			for len(batch) < cap(batch) {
				select {
				case next, ok := <-ch:
					if !ok {
						break drain
					}
					if next.Packet != nil {
						batch = append(batch, next)
					}
				default:
					break drain
				}
			}
			if debuglog.Enabled() {
				debuglog.Printf("transport", "writer dispatch %s packets=%d bytes=%d", debugLeg(payload.Leg), len(batch), payloadBatchBytes(batch))
			}
			err := writeUDPBatch(d.ctx, batch, d.packet)
			releasePayloads(batch)
			if err != nil {
				d.reportWriterError(ch, payload.Leg, err)
				return
			}
		}
	}
}

func (d *legWriterDispatcher) runTCPWriter(key writerKey, ch <-chan Payload) {
	defer d.wg.Done()
	batch := make([]Payload, 0, tcpPayloadBatchSize)
	for {
		select {
		case <-d.ctx.Done():
			d.releaseQueued(ch)
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			batch = append(batch[:0], payload)
		drain:
			for len(batch) < cap(batch) {
				select {
				case next, ok := <-ch:
					if !ok {
						break drain
					}
					if next.Packet != nil {
						batch = append(batch, next)
					}
				default:
					break drain
				}
			}
			if debuglog.Enabled() {
				debuglog.Printf("transport", "writer dispatch %s packets=%d bytes=%d", debugLeg(payload.Leg), len(batch), payloadBatchBytes(batch))
			}
			err := writeTCPBatch(d.ctx, batch, d.stream)
			if err != nil {
				d.reportWriterError(ch, payload.Leg, err)
				return
			}
		}
	}
}

func (d *legWriterDispatcher) reportWriterError(ch <-chan Payload, leg LegRef, err error) {
	debuglog.Printf("transport", "writer error %s err=%v", debugLeg(leg), err)
	metrics.IncCounter(metrics.TransportErrorsTotal,
		metrics.L("transport", kindLabel(leg.Kind)),
		metrics.L("operation", "write_dispatch"),
	)
	d.releaseQueued(ch)
	select {
	case d.errs <- err:
	case <-d.ctx.Done():
	default:
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

func writeUDPPayload(ctx context.Context, payload Payload, packet PacketTransport) error {
	if packet == nil || payload.Leg.EndpointID == "" || payload.Leg.RemoteAddr == nil {
		return ErrInvalidLeg
	}
	_, err := packet.WriteTo(ctx, payload.Leg.EndpointID, payload.Leg.RemoteAddr, payload.Packet.Payload)
	if err != nil && !errors.Is(err, context.Canceled) {
		if debuglog.Enabled() {
			debuglog.Printf("transport", "drop failed udp payload endpoint=%s remote=%v bytes=%d err=%v", payload.Leg.EndpointID, payload.Leg.RemoteAddr, len(payload.Packet.Payload), err)
		}
		return nil
	}
	return err
}

func writeUDPBatch(ctx context.Context, payloads []Payload, packet PacketTransport) error {
	if len(payloads) == 0 {
		return nil
	}
	first := payloads[0]
	if packet == nil || first.Leg.EndpointID == "" || first.Leg.RemoteAddr == nil {
		return ErrInvalidLeg
	}
	if len(payloads) == 1 {
		return writeUDPPayload(ctx, first, packet)
	}
	if batch, ok := packet.(packetBatchTransport); ok {
		_, err := batch.writeBatchTo(ctx, first.Leg.EndpointID, first.Leg.RemoteAddr, payloads)
		if err != nil && !errors.Is(err, context.Canceled) {
			if debuglog.Enabled() {
				debuglog.Printf("transport", "drop failed udp batch endpoint=%s remote=%v packets=%d bytes=%d err=%v", first.Leg.EndpointID, first.Leg.RemoteAddr, len(payloads), payloadBatchBytes(payloads), err)
			}
			return nil
		}
		return err
	}
	for _, payload := range payloads {
		if err := writeUDPPayload(ctx, payload, packet); err != nil {
			return err
		}
	}
	return nil
}

func writeTCPPayload(ctx context.Context, payload Payload, stream StreamTransport) error {
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
}

type streamPayloadBatchTransport interface {
	writePayloadBatch(ctx context.Context, connID string, payloads []Payload) error
}

func writeTCPBatch(ctx context.Context, payloads []Payload, stream StreamTransport) error {
	if len(payloads) == 0 {
		return nil
	}
	first := payloads[0]
	if stream == nil || first.Leg.ConnID == "" {
		releasePayloads(payloads)
		return ErrInvalidLeg
	}
	if batch, ok := stream.(streamPayloadBatchTransport); ok && len(payloads) > 1 {
		err := batch.writePayloadBatch(ctx, first.Leg.ConnID, payloads)
		if err != nil && !errors.Is(err, context.Canceled) {
			if debuglog.Enabled() {
				debuglog.Printf("transport", "drop stale tcp batch conn=%s packets=%d bytes=%d err=%v", first.Leg.ConnID, len(payloads), payloadBatchBytes(payloads), err)
			}
			return nil
		}
		return err
	}
	for i, payload := range payloads {
		err := writeTCPPayload(ctx, payload, stream)
		if payload.Packet != nil {
			payload.Packet.Release()
		}
		if err != nil {
			releasePayloads(payloads[i+1:])
			return err
		}
	}
	return nil
}

func udpPayloadBatchSize(packet PacketTransport, endpointID string) int {
	if batch, ok := packet.(packetBatchTransport); ok {
		if n := batch.batchSize(endpointID); n > 1 {
			return n
		}
	}
	return 1
}

func payloadBatchBytes(payloads []Payload) int {
	total := 0
	for _, payload := range payloads {
		if payload.Packet != nil {
			total += len(payload.Packet.Payload)
		}
	}
	return total
}

func releasePayloads(payloads []Payload) {
	for _, payload := range payloads {
		if payload.Packet != nil {
			payload.Packet.Release()
		}
	}
}
