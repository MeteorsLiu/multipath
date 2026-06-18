//go:build linux

package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/MeteorsLiu/multipath/internal/packetbuf"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const packetBatchIdealSize = 128

var errPacketBatchUnsupported = errors.New("transport: udp batch unsupported")

// ipv4.Message and ipv6.Message are aliases for the same socket message type.
var _ ipv6.Message = ipv4.Message{}

type udpBatchWriter interface {
	WriteBatch([]ipv6.Message, int) (int, error)
}

type udpBatchReader interface {
	ReadBatch([]ipv6.Message, int) (int, error)
}

type linuxPacketBatchSender struct {
	pc4    *ipv4.PacketConn
	pc6    *ipv6.PacketConn
	readPC udpBatchReader
	pool   sync.Pool
}

func newPacketBatcher(conn net.PacketConn) packetBatcher {
	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		return nil
	}
	pc4 := ipv4.NewPacketConn(udpConn)
	pc6 := ipv6.NewPacketConn(udpConn)
	sender := &linuxPacketBatchSender{
		pc4:    pc4,
		pc6:    pc6,
		readPC: packetBatchReadConn(udpConn, pc4, pc6),
	}
	sender.pool.New = func() any {
		msgs := make([]ipv6.Message, packetBatchIdealSize)
		for i := range msgs {
			msgs[i].Buffers = make(net.Buffers, 1)
		}
		return &msgs
	}
	return sender
}

func (s *linuxPacketBatchSender) batchSize() int {
	return packetBatchIdealSize
}

func (s *linuxPacketBatchSender) readBatchFrom(ctx context.Context, packets []*packetbuf.Packet, remotes []net.Addr) (int, error) {
	if s.readPC == nil {
		return 0, errPacketBatchUnsupported
	}
	if len(packets) == 0 {
		return 0, nil
	}
	msgs := s.getMessages()
	defer s.putMessages(msgs)
	if len(packets) > len(*msgs) {
		packets = packets[:len(*msgs)]
	}
	if len(remotes) > len(packets) {
		remotes = remotes[:len(packets)]
	}
	for i, packet := range packets {
		if packet == nil {
			continue
		}
		(*msgs)[i].Buffers[0] = packet.Payload
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	n, err := s.readPC.ReadBatch((*msgs)[:len(packets)], 0)
	if n > len(packets) {
		n = len(packets)
	}
	for i := 0; i < n; i++ {
		packets[i].SetLen((*msgs)[i].N)
		if i < len(remotes) {
			remotes[i] = (*msgs)[i].Addr
		}
	}
	return n, err
}

func (s *linuxPacketBatchSender) writeBatchTo(ctx context.Context, remote net.Addr, payloads []Payload) (int, error) {
	if len(payloads) == 0 {
		return 0, nil
	}
	udpAddr, ok := remote.(*net.UDPAddr)
	if !ok {
		return 0, errPacketBatchUnsupported
	}

	writer := udpBatchWriter(s.pc6)
	if udpAddr.IP.To4() != nil {
		writer = s.pc4
	}

	msgs := s.getMessages()
	defer s.putMessages(msgs)
	if len(payloads) > len(*msgs) {
		payloads = payloads[:len(*msgs)]
	}
	for i, payload := range payloads {
		if payload.Packet == nil {
			continue
		}
		(*msgs)[i].Addr = udpAddr
		(*msgs)[i].Buffers[0] = payload.Packet.Payload
	}

	written := 0
	for written < len(payloads) {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		n, err := writer.WriteBatch((*msgs)[written:len(payloads)], 0)
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

func (s *linuxPacketBatchSender) getMessages() *[]ipv6.Message {
	return s.pool.Get().(*[]ipv6.Message)
}

func (s *linuxPacketBatchSender) putMessages(msgs *[]ipv6.Message) {
	for i := range *msgs {
		(*msgs)[i] = ipv6.Message{Buffers: (*msgs)[i].Buffers}
	}
	s.pool.Put(msgs)
}

func packetBatchReadConn(conn *net.UDPConn, pc4 *ipv4.PacketConn, pc6 *ipv6.PacketConn) udpBatchReader {
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return pc4
	}
	if addr.IP != nil && !addr.IP.IsUnspecified() && addr.IP.To4() == nil {
		return pc6
	}
	return pc4
}
