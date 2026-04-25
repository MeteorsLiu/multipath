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
		go s.readLoop(ctx, connID, conn, writer)
	}
	s.mu.Unlock()

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
	conn, err := dialer.DialContext(ctx, "tcp", remote)
	if err != nil {
		return LegRef{}, err
	}

	connID := s.addConn(conn)
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
	n, err := writeBuffersFull(conn, buffers)
	return payloadBytesWritten(n), err
}

func (s *Stream) Close(ctx context.Context, connID string) error {
	s.mu.Lock()
	conn := s.conns[connID]
	delete(s.conns, connID)
	s.mu.Unlock()
	if conn == nil {
		return ErrUnknownConn
	}

	return conn.Close()
}

func (s *Stream) acceptLoop(ctx context.Context, writer PacketWriter) error {
	for {
		if tcpListener, ok := s.listener.(*net.TCPListener); ok {
			if err := tcpListener.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
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
			return err
		}

		connID := s.addConn(conn)
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
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
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
			return
		}

		frameLen := int(binary.BigEndian.Uint16(header[:]))
		if frameLen == 0 || frameLen > maxStreamFrameLen {
			return
		}

		packet := packetbuf.Acquire(frameLen)
		if _, err := io.ReadFull(conn, packet.Payload); err != nil {
			packet.Release()
			return
		}
		leg := LegRef{
			Kind:   KindTCP,
			ConnID: connID,
		}

		if err := writer.WriteTo(ctx, leg, packet); err != nil {
			return
		}
	}
}

func (s *Stream) addConn(conn net.Conn) string {
	connID := fmt.Sprintf("tcp-%d", s.nextID.Add(1))
	s.mu.Lock()
	s.conns[connID] = conn
	s.mu.Unlock()
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
