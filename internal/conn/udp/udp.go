/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 * Copyright (C) 2025 MeteorsLiu. All Rights Reserved.
 */
package udp

import (
	"errors"
	"net"
	"runtime"
	"strconv"
	"sync"
	"syscall"

	"github.com/MeteorsLiu/multipath/internal/conn"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	udpAddrPool = sync.Pool{
		New: func() any {
			return &net.UDPAddr{
				IP: make([]byte, 16),
			}
		},
	}

	ipv4MsgsPool = sync.Pool{
		New: func() any {
			msgs := make([]ipv4.Message, 1024)
			for i := range msgs {
				msgs[i].Buffers = make(net.Buffers, 1)
				msgs[i].OOB = make([]byte, srcControlSize)
			}
			return &msgs
		},
	}

	ipv6MsgsPool = sync.Pool{
		New: func() any {
			msgs := make([]ipv6.Message, 1024)
			for i := range msgs {
				msgs[i].Buffers = make(net.Buffers, 1)
				msgs[i].OOB = make([]byte, srcControlSize)
			}
			return &msgs
		},
	}
)

type udpBind struct {
	port int

	v4pc     *ipv4.PacketConn
	v6pc     *ipv6.PacketConn
	endpoint *StdNetEndpoint

	conn *net.UDPConn
}

func NewUDP4(listenPort int) (conn.BatchConn, error) {
	v4conn, port, err := listenNet("udp4", listenPort)
	if err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) {
		return nil, err
	}

	u := &udpBind{port: port}

	if runtime.GOOS == "linux" {
		u.v4pc = ipv4.NewPacketConn(v4conn)
	}
	u.conn = v4conn

	return u, nil
}

func NewUDP6(listenPort int) (conn.BatchConn, error) {
	v6conn, port, err := listenNet("udp6", listenPort)
	if err != nil && !errors.Is(err, syscall.EAFNOSUPPORT) {
		return nil, err
	}

	u := &udpBind{port: port}

	if runtime.GOOS == "linux" {
		u.v6pc = ipv6.NewPacketConn(v6conn)
	}
	u.conn = v6conn

	return u, nil
}

func listenNet(network string, port int) (*net.UDPConn, int, error) {
	conn, err := net.ListenPacket(network, ":"+strconv.Itoa(port))
	if err != nil {
		return nil, 0, err
	}

	// Retrieve port.
	laddr := conn.LocalAddr()
	uaddr, err := net.ResolveUDPAddr(
		laddr.Network(),
		laddr.String(),
	)
	if err != nil {
		return nil, 0, err
	}
	return conn.(*net.UDPConn), uaddr.Port, nil
}

func (s *udpBind) ReadBatch(b [][]byte) (int64, error) {
	if s.v6pc != nil {
		return s.readBatchV6(b)
	}
	return s.readBatchV4(b)
}

func (s *udpBind) readBatchV4(bufs [][]byte) (int64, error) {
	msgs := ipv4MsgsPool.Get().(*[]ipv4.Message)
	defer ipv4MsgsPool.Put(msgs)
	for i := range bufs {
		(*msgs)[i].Buffers[0] = bufs[i]
	}
	var err error
	var numMsgs int
	if runtime.GOOS == "linux" {
		numMsgs, err = s.v4pc.ReadBatch(*msgs, 0)
		if err != nil {
			return 0, err
		}
	} else {
		msg := &(*msgs)[0]
		msg.N, msg.NN, _, msg.Addr, err = s.conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
		if err != nil {
			return 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		msg := &(*msgs)[i]
		sum += int64(msg.N)
	}

	return sum, nil
}

func (s *udpBind) readBatchV6(bufs [][]byte) (int64, error) {
	msgs := ipv6MsgsPool.Get().(*[]ipv6.Message)
	defer ipv6MsgsPool.Put(msgs)
	for i := range bufs {
		(*msgs)[i].Buffers[0] = bufs[i]
	}
	var err error
	var numMsgs int
	if runtime.GOOS == "linux" {
		numMsgs, err = s.v6pc.ReadBatch(*msgs, 0)
		if err != nil {
			return 0, err
		}
	} else {
		msg := &(*msgs)[0]
		msg.N, msg.NN, _, msg.Addr, err = s.conn.ReadMsgUDP(msg.Buffers[0], msg.OOB)
		if err != nil {
			return 0, err
		}
		numMsgs = 1
	}
	var sum int64
	for i := 0; i < numMsgs; i++ {
		msg := &(*msgs)[i]
		sum += int64(msg.N)
	}

	return sum, nil
}

func (s *udpBind) Close() error {
	var err1, err2 error
	if s.v4pc != nil {
		err1 = s.v4pc.Close()
	}
	if s.v6pc != nil {
		err2 = s.v6pc.Close()
	}
	err3 := s.conn.Close()

	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}

func (s *udpBind) WriteBatch(bufs [][]byte) (int64, error) {
	if s.v6pc != nil {
		return s.send6(bufs)
	}
	return s.send4(bufs)
}

func (s *udpBind) send4(bufs [][]byte) (int64, error) {
	ua := udpAddrPool.Get().(*net.UDPAddr)
	as4 := s.endpoint.DstIP().As4()
	copy(ua.IP, as4[:])
	ua.IP = ua.IP[:4]
	ua.Port = int(s.endpoint.Port())
	msgs := ipv4MsgsPool.Get().(*[]ipv4.Message)
	for i, buf := range bufs {
		(*msgs)[i].Buffers[0] = buf
		(*msgs)[i].Addr = ua
		setSrcControl(&(*msgs)[i].OOB, s.endpoint)
	}
	var (
		n     int
		err   error
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v4pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, (*msgs)[i].OOB, ua)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil {
				break
			}
		}
	}
	udpAddrPool.Put(ua)
	ipv4MsgsPool.Put(msgs)
	return sent, err
}

func (s *udpBind) send6(bufs [][]byte) (int64, error) {
	ua := udpAddrPool.Get().(*net.UDPAddr)
	as16 := s.endpoint.DstIP().As16()
	copy(ua.IP, as16[:])
	ua.IP = ua.IP[:16]
	ua.Port = int(s.endpoint.Port())
	msgs := ipv6MsgsPool.Get().(*[]ipv6.Message)
	for i, buf := range bufs {
		(*msgs)[i].Buffers[0] = buf
		(*msgs)[i].Addr = ua
		setSrcControl(&(*msgs)[i].OOB, s.endpoint)
	}
	var (
		n     int
		err   error
		start int
		sent  int64
	)
	if runtime.GOOS == "linux" {
		for {
			n, err = s.v6pc.WriteBatch((*msgs)[start:len(bufs)], 0)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil || n == len((*msgs)[start:len(bufs)]) {
				break
			}
			start += n
		}
	} else {
		for i, buf := range bufs {
			n, _, err = s.conn.WriteMsgUDP(buf, (*msgs)[i].OOB, ua)
			if n > 0 {
				sent += int64(n)
			}
			if err != nil {
				break
			}
		}
	}
	udpAddrPool.Put(ua)
	ipv6MsgsPool.Put(msgs)
	return sent, err
}
