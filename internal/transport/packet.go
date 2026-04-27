package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const defaultPacketBufferSize = 64 * 1024

var (
	ErrUnknownEndpoint = errors.New("transport: unknown packet endpoint")
	ErrNilPacketConn   = errors.New("transport: nil packet conn")
)

type PacketEndpoint struct {
	ID   string
	Conn net.PacketConn
}

type Packet struct {
	mu        sync.RWMutex
	endpoints map[string]net.PacketConn
	bufSize   int
}

func NewPacket(endpoints ...PacketEndpoint) (*Packet, error) {
	p := &Packet{
		endpoints: make(map[string]net.PacketConn, len(endpoints)),
		bufSize:   defaultPacketBufferSize,
	}
	for _, endpoint := range endpoints {
		if endpoint.Conn == nil {
			return nil, ErrNilPacketConn
		}
		if endpoint.ID == "" {
			return nil, fmt.Errorf("%w: empty endpoint id", ErrUnknownEndpoint)
		}
		p.endpoints[endpoint.ID] = endpoint.Conn
	}
	return p, nil
}

func (p *Packet) Run(ctx context.Context, writer PacketWriter) error {
	p.mu.RLock()
	endpoints := make([]PacketEndpoint, 0, len(p.endpoints))
	for id, conn := range p.endpoints {
		endpoints = append(endpoints, PacketEndpoint{ID: id, Conn: conn})
	}
	p.mu.RUnlock()
	debuglog.Printf("transport/udp", "run endpoints=%d", len(endpoints))

	errCh := make(chan error, len(endpoints))
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(endpoint PacketEndpoint) {
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
	conn := p.endpoints[endpointID]
	p.mu.RUnlock()
	if conn == nil {
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
	n, err := conn.WriteTo(payload, remote)
	if err != nil {
		debuglog.Printf("transport/udp", "write endpoint=%s remote=%v bytes=%d err=%v", endpointID, remote, len(payload), err)
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "udp"),
			metrics.L("operation", "write"),
		)
		return n, err
	}
	debuglog.Printf("transport/udp", "write endpoint=%s remote=%v bytes=%d", endpointID, remote, n)
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
	return n, nil
}

func (p *Packet) readLoop(ctx context.Context, endpoint PacketEndpoint, writer PacketWriter) error {
	bufSize := p.bufSize
	if bufSize <= 0 {
		bufSize = defaultPacketBufferSize
	}

	for {
		packet := packetbuf.Acquire(bufSize)
		if err := endpoint.Conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			packet.Release()
			debuglog.Printf("transport/udp", "read endpoint=%s deadline err=%v", endpoint.ID, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "read_deadline"),
			)
			return err
		}

		n, remote, err := endpoint.Conn.ReadFrom(packet.Payload)
		if err != nil {
			packet.Release()
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return nil
				default:
					continue
				}
			}
			debuglog.Printf("transport/udp", "read endpoint=%s err=%v", endpoint.ID, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "read"),
			)
			return err
		}
		leg := LegRef{
			Kind:       KindUDP,
			EndpointID: endpoint.ID,
			RemoteAddr: remote,
		}
		packet.SetLen(n)
		debuglog.Printf("transport/udp", "read endpoint=%s remote=%v bytes=%d", endpoint.ID, remote, n)
		metrics.IncCounter(metrics.TransportPacketsTotal,
			metrics.L("transport", "udp"),
			metrics.L("direction", "rx"),
			metrics.L("endpoint", endpoint.ID),
		)
		metrics.AddCounter(metrics.TransportBytesTotal, uint64(n),
			metrics.L("transport", "udp"),
			metrics.L("direction", "rx"),
			metrics.L("endpoint", endpoint.ID),
		)

		if err := writer.WriteTo(ctx, leg, packet); err != nil {
			debuglog.Printf("transport/udp", "deliver endpoint=%s remote=%v bytes=%d err=%v", endpoint.ID, remote, n, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "udp"),
				metrics.L("operation", "deliver"),
			)
			return err
		}
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
