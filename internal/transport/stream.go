package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/packetbuf"
)

const maxStreamFrameLen = 64 * 1024

var (
	ErrUnknownConn   = errors.New("transport: unknown stream conn")
	ErrFrameTooLarge = errors.New("transport: stream frame too large")
)

type Stream struct {
	listener net.Listener

	mu     sync.RWMutex
	conns  map[string]net.Conn
	writer PacketWriter
	nextID atomic.Uint64
}

func NewStream(listener net.Listener) *Stream {
	return &Stream{
		listener: listener,
		conns:    make(map[string]net.Conn),
	}
}

func (s *Stream) Run(ctx context.Context, writer PacketWriter) error {
	s.mu.Lock()
	s.writer = writer
	for connID, conn := range s.conns {
		debuglog.Printf("transport/tcp", "start existing read_loop conn=%s remote=%v", connID, debugRemoteAddr(conn))
		go s.readLoop(ctx, connID, conn, writer)
	}
	s.mu.Unlock()
	debuglog.Printf("transport/tcp", "run listener=%v", s.listener != nil)

	errCh := make(chan error, 1)
	if s.listener != nil {
		go func() {
			errCh <- s.acceptLoop(ctx, writer)
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Stream) Dial(ctx context.Context, remote string) (LegRef, error) {
	var dialer net.Dialer
	debuglog.Printf("transport/tcp", "dial remote=%s", remote)
	conn, err := dialer.DialContext(ctx, "tcp", remote)
	if err != nil {
		debuglog.Printf("transport/tcp", "dial remote=%s err=%v", remote, err)
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "tcp"),
			metrics.L("operation", "dial"),
		)
		return LegRef{}, err
	}

	connID := s.addConn(conn)
	debuglog.Printf("transport/tcp", "dial ok remote=%s conn=%s local=%v", remote, connID, debugLocalAddr(conn))
	writer := s.currentWriter()
	if writer != nil {
		go s.readLoop(ctx, connID, conn, writer)
	}

	return LegRef{
		Kind:   KindTCP,
		ConnID: connID,
	}, nil
}

func (s *Stream) Write(ctx context.Context, connID string, payload []byte) (int, error) {
	if len(payload) == 0 || len(payload) > maxStreamFrameLen-1 {
		return 0, ErrFrameTooLarge
	}

	s.mu.RLock()
	conn := s.conns[connID]
	s.mu.RUnlock()
	if conn == nil {
		debuglog.Printf("transport/tcp", "write unknown conn=%s bytes=%d", connID, len(payload))
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "tcp"),
			metrics.L("operation", "write_unknown_conn"),
		)
		return 0, ErrUnknownConn
	}

	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	buffers := net.Buffers{header[:], payload}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, err
	}
	n, err := writeBuffersFull(conn, buffers)
	if err != nil {
		if isTimeout(err) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		}
		debuglog.Printf("transport/tcp", "write conn=%s bytes=%d err=%v", connID, len(payload), err)
		metrics.IncCounter(metrics.TransportErrorsTotal,
			metrics.L("transport", "tcp"),
			metrics.L("operation", "write"),
		)
		return payloadBytesWritten(n), err
	}
	written := payloadBytesWritten(n)
	if debuglog.Enabled() {
		debuglog.Printf("transport/tcp", "write conn=%s bytes=%d", connID, written)
	}
	metrics.IncCounter(metrics.TransportPacketsTotal,
		metrics.L("transport", "tcp"),
		metrics.L("direction", "tx"),
		metrics.L("endpoint", ""),
	)
	metrics.AddCounter(metrics.TransportBytesTotal, uint64(written),
		metrics.L("transport", "tcp"),
		metrics.L("direction", "tx"),
		metrics.L("endpoint", ""),
	)
	return payloadBytesWritten(n), err
}

func (s *Stream) Close(ctx context.Context, connID string) error {
	s.mu.Lock()
	conn := s.conns[connID]
	delete(s.conns, connID)
	s.mu.Unlock()
	if conn == nil {
		debuglog.Printf("transport/tcp", "close unknown conn=%s", connID)
		return ErrUnknownConn
	}

	err := conn.Close()
	debuglog.Printf("transport/tcp", "close conn=%s err=%v", connID, err)
	return err
}

func (s *Stream) acceptLoop(ctx context.Context, writer PacketWriter) error {
	for {
		if tcpListener, ok := s.listener.(*net.TCPListener); ok {
			if err := tcpListener.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				debuglog.Printf("transport/tcp", "accept deadline err=%v", err)
				return err
			}
		}

		conn, err := s.listener.Accept()
		if err != nil {
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return nil
				default:
					continue
				}
			}
			debuglog.Printf("transport/tcp", "accept err=%v", err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "accept"),
			)
			return err
		}

		connID := s.addConn(conn)
		debuglog.Printf("transport/tcp", "accept conn=%s remote=%v local=%v", connID, debugRemoteAddr(conn), debugLocalAddr(conn))
		go s.readLoop(ctx, connID, conn, writer)
	}
}

func (s *Stream) readLoop(ctx context.Context, connID string, conn net.Conn, writer PacketWriter) {
	defer func() {
		s.mu.Lock()
		if s.conns[connID] == conn {
			delete(s.conns, connID)
		}
		s.mu.Unlock()
		_ = conn.Close()
		debuglog.Printf("transport/tcp", "read_loop exit conn=%s remote=%v", connID, debugRemoteAddr(conn))
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			debuglog.Printf("transport/tcp", "read deadline conn=%s err=%v", connID, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "read_deadline"),
			)
			return
		}

		var header [2]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			if isTimeout(err) {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			debuglog.Printf("transport/tcp", "read header conn=%s err=%v", connID, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "read_header"),
			)
			return
		}

		frameLen := int(binary.BigEndian.Uint16(header[:]))
		if frameLen == 0 || frameLen > maxStreamFrameLen {
			debuglog.Printf("transport/tcp", "invalid frame_len conn=%s frame_len=%d", connID, frameLen)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "read_invalid_frame_len"),
			)
			return
		}

		packet := packetbuf.Acquire(frameLen)
		if _, err := io.ReadFull(conn, packet.Payload); err != nil {
			packet.Release()
			debuglog.Printf("transport/tcp", "read frame conn=%s frame_len=%d err=%v", connID, frameLen, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "read_frame"),
			)
			return
		}
		leg := LegRef{
			Kind:   KindTCP,
			ConnID: connID,
		}

		if debuglog.Enabled() {
			debuglog.Printf("transport/tcp", "read conn=%s bytes=%d", connID, frameLen)
		}
		metrics.IncCounter(metrics.TransportPacketsTotal,
			metrics.L("transport", "tcp"),
			metrics.L("direction", "rx"),
			metrics.L("endpoint", ""),
		)
		metrics.AddCounter(metrics.TransportBytesTotal, uint64(frameLen),
			metrics.L("transport", "tcp"),
			metrics.L("direction", "rx"),
			metrics.L("endpoint", ""),
		)
		if err := writer.WriteTo(ctx, leg, packet); err != nil {
			debuglog.Printf("transport/tcp", "deliver conn=%s bytes=%d err=%v", connID, frameLen, err)
			metrics.IncCounter(metrics.TransportErrorsTotal,
				metrics.L("transport", "tcp"),
				metrics.L("operation", "deliver"),
			)
			return
		}
	}
}

func (s *Stream) addConn(conn net.Conn) string {
	connID := fmt.Sprintf("tcp-%d", s.nextID.Add(1))
	s.mu.Lock()
	s.conns[connID] = conn
	s.mu.Unlock()
	debuglog.Printf("transport/tcp", "add_conn conn=%s remote=%v local=%v", connID, debugRemoteAddr(conn), debugLocalAddr(conn))
	return connID
}

func (s *Stream) currentWriter() PacketWriter {
	s.mu.RLock()
	writer := s.writer
	s.mu.RUnlock()
	return writer
}

func writeBuffersFull(conn net.Conn, buffers net.Buffers) (int64, error) {
	var written int64
	for len(buffers) > 0 {
		n, err := buffers.WriteTo(conn)
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func payloadBytesWritten(frameBytes int64) int {
	if frameBytes <= 2 {
		return 0
	}
	return int(frameBytes - 2)
}
