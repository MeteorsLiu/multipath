package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const defaultPacketBufferSize = 64 * 1024
const udpSocketBufferSize = 32 * 1024 * 1024

var (
	ErrUnknownEndpoint = errors.New("transport: unknown packet endpoint")
	ErrNilPacketConn   = errors.New("transport: nil packet conn")
)

type PacketEndpoint struct {
	ID   string
	Conn net.PacketConn
}

type packetEndpointState struct {
	conn  net.PacketConn
	batch packetBatcher
}

type packetBatcher interface {
	batchSize() int
	writeBatchTo(ctx context.Context, remote net.Addr, payloads []Payload) (int, error)
	readBatchFrom(ctx context.Context, packets []*packetbuf.Packet, remotes []net.Addr) (int, error)
}

type packetReadEndpoint struct {
	id    string
	conn  net.PacketConn
	batch packetBatcher
}

type Packet struct {
	mu        sync.RWMutex
	endpoints map[string]packetEndpointState
	bufSize   int
}

func NewPacket(endpoints ...PacketEndpoint) (*Packet, error) {
	p := &Packet{
		endpoints: make(map[string]packetEndpointState, len(endpoints)),
		bufSize:   defaultPacketBufferSize,
	}
	for _, endpoint := range endpoints {
		if endpoint.Conn == nil {
			return nil, ErrNilPacketConn
		}
		if endpoint.ID == "" {
			return nil, fmt.Errorf("%w: empty endpoint id", ErrUnknownEndpoint)
		}
		tunePacketConn(endpoint.ID, endpoint.Conn)
		p.endpoints[endpoint.ID] = packetEndpointState{
			conn:  endpoint.Conn,
			batch: newPacketBatcher(endpoint.Conn),
		}
	}
	return p, nil
}

func (p *Packet) Run(ctx context.Context, writer PacketWriter) error {
	p.mu.RLock()
	endpoints := make([]packetReadEndpoint, 0, len(p.endpoints))
	closeEndpoints := make([]PacketEndpoint, 0, len(p.endpoints))
	for id, endpoint := range p.endpoints {
		endpoints = append(endpoints, packetReadEndpoint{id: id, conn: endpoint.conn, batch: endpoint.batch})
		closeEndpoints = append(closeEndpoints, PacketEndpoint{ID: id, Conn: endpoint.conn})
	}
	p.mu.RUnlock()
	debuglog.Printf("transport/udp", "run endpoints=%d", len(endpoints))

	cancelCloseDone := closePacketEndpointsOnCancel(ctx, closeEndpoints)
	defer cancelCloseDone()

	errCh := make(chan error, len(endpoints))
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(endpoint packetReadEndpoint) {
			defer wg.Done()
			if err := p.readLoop(ctx, endpoint, writer); err != nil {
				errCh <- err
			}
		}(endpoint)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		<-done
		return ctx.Err()
	case err := <-errCh:
		return err
	case <-done:
		return nil
	}
}

func (p *Packet) WriteTo(ctx context.Context, endpointID string, remote net.Addr, payload []byte) (int, error) {
	p.mu.RLock()
	endpoint := p.endpoints[endpointID]
	p.mu.RUnlock()
	if endpoint.conn == nil {
		debuglog.Printf("transport/udp", "write unknown endpoint=%s remote=%v bytes=%d", endpointID, remote, len(payload))
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write_unknown_endpoint"),
		)
		return 0, ErrUnknownEndpoint
	}

	select {
	case <-ctx.Done():
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write_ctx_done"),
		)
		return 0, ctx.Err()
	default:
	}
	return p.writeOneToEndpoint(endpointID, endpoint.conn, remote, payload)
}

func (p *Packet) writeBatchTo(ctx context.Context, endpointID string, remote net.Addr, payloads []Payload) (int, error) {
	if len(payloads) == 0 {
		return 0, nil
	}

	p.mu.RLock()
	endpoint := p.endpoints[endpointID]
	p.mu.RUnlock()
	if endpoint.conn == nil {
		debuglog.Printf("transport/udp", "write_batch unknown endpoint=%s remote=%v packets=%d", endpointID, remote, len(payloads))
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write_unknown_endpoint"),
		)
		return 0, ErrUnknownEndpoint
	}

	select {
	case <-ctx.Done():
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write_ctx_done"),
		)
		return 0, ctx.Err()
	default:
	}

	if endpoint.batch == nil || len(payloads) == 1 {
		return p.writeBatchSlow(ctx, endpointID, endpoint.conn, remote, payloads)
	}

	n, err := endpoint.batch.writeBatchTo(ctx, remote, payloads)
	if n > len(payloads) {
		n = len(payloads)
	}
	for i := 0; i < n; i++ {
		p.recordUDPWrite(endpointID, len(payloads[i].Packet.Payload))
	}
	if err == nil {
		return n, nil
	}
	if ctx.Err() != nil {
		return n, ctx.Err()
	}
	debuglog.Printf("transport/udp", "write_batch endpoint=%s remote=%v sent=%d packets=%d err=%v", endpointID, remote, n, len(payloads), err)
	metrics.IncCounter(metrics.TransportErrorsTotal,
		metrics.L("transport", "udp"),
		metrics.L("operation", "write_batch"),
	)

	slowN, slowErr := p.writeBatchSlow(ctx, endpointID, endpoint.conn, remote, payloads[n:])
	return n + slowN, slowErr
}

func (p *Packet) batchSize(endpointID string) int {
	p.mu.RLock()
	endpoint := p.endpoints[endpointID]
	p.mu.RUnlock()
	if endpoint.batch == nil {
		return 1
	}
	if n := endpoint.batch.batchSize(); n > 1 {
		return n
	}
	return 1
}

func (p *Packet) writeBatchSlow(ctx context.Context, endpointID string, conn net.PacketConn, remote net.Addr, payloads []Payload) (int, error) {
	written := 0
	var firstErr error
	for _, payload := range payloads {
		if payload.Packet == nil {
			continue
		}
		if _, err := p.writeOneToEndpoint(endpointID, conn, remote, payload.Packet.Payload); err != nil {
			if errors.Is(err, context.Canceled) {
				return written, err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written++
	}
	return written, firstErr
}

func (p *Packet) writeOneToEndpoint(endpointID string, conn net.PacketConn, remote net.Addr, payload []byte) (int, error) {
	n, err := conn.WriteTo(payload, remote)
	if err != nil {
		debuglog.Printf("transport/udp", "write endpoint=%s remote=%v bytes=%d err=%v", endpointID, remote, len(payload), err)
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write"),
		)
		return n, err
	}
	if debuglog.Enabled() {
		debuglog.Printf("transport/udp", "write endpoint=%s remote=%v bytes=%d", endpointID, remote, n)
	}
	p.recordUDPWrite(endpointID, n)
	return n, nil
}

func (p *Packet) recordUDPWrite(endpointID string, n int) {
	metrics.IncCounter(metrics.TransportPacketsTotal,
		metrics.L("transport", "udp"),
		metrics.L("direction", "tx"),
		metrics.L("endpoint", endpointID),
	)
	metrics.AddCounter(metrics.TransportBytesTotal, uint64(n),
		metrics.L("transport", "udp"),
		metrics.L("direction", "tx"),
		metrics.L("endpoint", endpointID),
	)
}

func (p *Packet) readLoop(ctx context.Context, endpoint packetReadEndpoint, writer PacketWriter) error {
	bufSize := p.bufSize
	if bufSize <= 0 {
		bufSize = defaultPacketBufferSize
	}
	if endpoint.batch != nil {
		if batchSize := endpoint.batch.batchSize(); batchSize > 1 {
			return p.readBatchLoop(ctx, endpoint, writer, bufSize, batchSize)
		}
	}
	return p.readOneLoop(ctx, endpoint, writer, bufSize)
}

func (p *Packet) readOneLoop(ctx context.Context, endpoint packetReadEndpoint, writer PacketWriter, bufSize int) error {
	for {
		packet := packetbuf.Acquire(bufSize)
		n, remote, err := endpoint.conn.ReadFrom(packet.Payload)
		if err != nil {
			packet.Release()
			if ctx.Err() != nil {
				return nil
			}
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return nil
				default:
					continue
				}
			}
			debuglog.Printf("transport/udp", "read endpoint=%s err=%v", endpoint.id, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "read"),
			)
			return err
		}
		packet.SetLen(n)
		if err := p.deliverUDPPacket(ctx, endpoint.id, remote, packet, writer); err != nil {
			return err
		}
	}
}

func (p *Packet) readBatchLoop(ctx context.Context, endpoint packetReadEndpoint, writer PacketWriter, bufSize, batchSize int) error {
	packets := make([]*packetbuf.Packet, batchSize)
	remotes := make([]net.Addr, batchSize)
	for {
		for i := range packets {
			if packets[i] == nil {
				packets[i] = packetbuf.Acquire(bufSize)
			}
			remotes[i] = nil
		}
		n, err := endpoint.batch.readBatchFrom(ctx, packets, remotes)
		if n > len(packets) {
			n = len(packets)
		}
		if n == 0 {
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				releasePackets(packets)
				return nil
			}
			if isTimeout(err) {
				continue
			}
			releasePackets(packets)
			debuglog.Printf("transport/udp", "read_batch endpoint=%s err=%v", endpoint.id, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "read_batch"),
			)
			return err
		}
		for i := 0; i < n; i++ {
			packet := packets[i]
			packets[i] = nil
			if packet == nil {
				continue
			}
			if remotes[i] == nil {
				packet.Release()
				continue
			}
			if err := p.deliverUDPPacket(ctx, endpoint.id, remotes[i], packet, writer); err != nil {
				releasePackets(packets)
				return err
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				releasePackets(packets)
				return nil
			}
			debuglog.Printf("transport/udp", "read_batch endpoint=%s delivered=%d err=%v", endpoint.id, n, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "read_batch"),
			)
			return err
		}
	}
}

func (p *Packet) deliverUDPPacket(ctx context.Context, endpointID string, remote net.Addr, packet *packetbuf.Packet, writer PacketWriter) error {
	leg := LegRef{
		Kind:       KindUDP,
		EndpointID: endpointID,
		RemoteAddr: remote,
	}
	if debuglog.Enabled() {
		debuglog.Printf("transport/udp", "read endpoint=%s remote=%v bytes=%d", endpointID, remote, len(packet.Payload))
	}
	metrics.IncCounter(metrics.TransportPacketsTotal,
		metrics.L("transport", "udp"),
		metrics.L("direction", "rx"),
		metrics.L("endpoint", endpointID),
	)
	metrics.AddCounter(metrics.TransportBytesTotal, uint64(len(packet.Payload)),
		metrics.L("transport", "udp"),
		metrics.L("direction", "rx"),
		metrics.L("endpoint", endpointID),
	)

	if err := writer.WriteTo(ctx, leg, packet); err != nil {
		debuglog.Printf("transport/udp", "deliver endpoint=%s remote=%v bytes=%d err=%v", endpointID, remote, len(packet.Payload), err)
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "deliver"),
		)
		return err
	}
	return nil
}

func releasePackets(packets []*packetbuf.Packet) {
	for i, packet := range packets {
		if packet != nil {
			packet.Release()
			packets[i] = nil
		}
	}
}

func closePacketEndpointsOnCancel(ctx context.Context, endpoints []PacketEndpoint) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			for _, endpoint := range endpoints {
				_ = endpoint.Conn.Close()
			}
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
