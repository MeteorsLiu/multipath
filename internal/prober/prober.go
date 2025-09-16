package prober

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/MeteorsLiu/multipath/internal/conn/udpmux/protocol"
	"github.com/MeteorsLiu/multipath/internal/mempool"
	"github.com/google/uuid"
)

const (
	NonceSize          = 8
	_defaultTimeout    = 15
	_defaultTimeoutDur = _defaultTimeout * time.Second
	_probeInterval     = 300 * time.Millisecond
)

type Event int

const (
	Normal Event = iota
	Disconnected
	Lost
)

func (e Event) String() string {
	switch e {
	case Normal:
		return "normal"
	case Disconnected:
		return "disconnected"
	case Lost:
		return "lost"
	}
	return "unknown"
}

type packetInfo struct {
	startTime time.Time
	isTimeout bool
}

type gcPacket struct {
	nonce   uint64
	elasped time.Duration
}

type Prober struct {
	on       func(Event)
	state    Event
	proberId uuid.UUID

	in             chan *mempool.Buffer
	out            chan *mempool.Buffer
	ctx            context.Context
	addr           string
	lost           float64
	debit          float64
	deadline       *time.Timer
	ticker         *time.Ticker // 固定间隔发送器
	currentTimeout time.Duration

	// RTT估计
	srtt   time.Duration
	rttvar time.Duration
	minRtt time.Duration // 最小RTT

	packetMap        map[uint64]*packetInfo
	lastMaxStartTime int64
}

func New(ctx context.Context, addr string, on func(Event)) *Prober {
	p := &Prober{
		on:             on,
		ctx:            ctx,
		addr:           addr,
		in:             make(chan *mempool.Buffer, 128),
		out:            make(chan *mempool.Buffer, 128),
		currentTimeout: _defaultTimeoutDur, // 初始超时时间15秒
		deadline:       new(time.Timer),
		packetMap:      make(map[uint64]*packetInfo),
	}
	return p
}

func (p *Prober) In() chan<- *mempool.Buffer {
	return p.in
}

func (p *Prober) Out() <-chan *mempool.Buffer {
	return p.out
}

func (p *Prober) sendProbePacket() {
	packet := mempool.GetWithHeader(NonceSize, protocol.HeaderSize+ProbeHeaderSize)
	packet.ReadFrom(rand.Reader)

	// skip epoch id, it's reset to zero by GetWithHeader
	packet.WriteAt(p.proberId[:], protocol.HeaderSize+1)

	nonce := binary.LittleEndian.Uint64(packet.Bytes())

	p.packetMap[nonce] = &packetInfo{startTime: time.Now()}

	p.out <- packet

	// 设置超时定时器
	if p.deadline.C == nil {
		p.deadline = time.NewTimer(p.currentTimeout)
	} else {
		p.deadline.Reset(p.currentTimeout)
	}
}

func (p *Prober) markTimeout() (isTimeout bool) {
	var timeHeap maxTimeHeap

	if p.lastMaxStartTime > 0 && time.Now().Unix()-p.lastMaxStartTime >= _defaultTimeout {
		p.switchState(Disconnected)
		isTimeout = true
		p.lastMaxStartTime = 0
	}
	// too many on flight
	needGC := len(p.packetMap) > 100

	for nonce, pkt := range p.packetMap {
		elapsed := time.Since(pkt.startTime)

		// if one of packet has no reply over 15 seconds, mark it disconnected.
		if elapsed >= _defaultTimeoutDur {
			p.switchState(Disconnected)
			isTimeout = true
		}

		if !pkt.isTimeout && elapsed >= p.currentTimeout {
			pkt.isTimeout = true
			isTimeout = true
		}

		if needGC {
			heap.Push(&timeHeap, &gcPacket{
				nonce:   nonce,
				elasped: elapsed,
			})
			p.lastMaxStartTime = max(p.lastMaxStartTime, pkt.startTime.Unix())
		}
	}

	if !needGC {
		return
	}

	const exceedSize = 50

	for len(p.packetMap) > exceedSize {
		pkt := heap.Pop(&timeHeap).(*gcPacket)
		delete(p.packetMap, pkt.nonce)
	}

	return
}

func (p *Prober) recvProbePacket(packet *mempool.Buffer) {
	defer mempool.Put(packet)

	nonce := binary.LittleEndian.Uint64(packet.Bytes())

	info, ok := p.packetMap[nonce]
	if !ok {
		// has been GC or unknown
		return
	}
	defer delete(p.packetMap, nonce)

	elapsedTimeDur := time.Since(info.startTime)

	// check twice, this aims to avoid the case receiving probe packet and reaching deadline concurrently.
	isTimeout := info.isTimeout || elapsedTimeDur >= p.currentTimeout

	if isTimeout {
		if p.debit > 0 {
			p.debit = 10
		}
		return
	}
	p.lastMaxStartTime = 0
	// clear timeout
	p.deadline.Stop()

	// 更新RTT估计
	rtt := elapsedTimeDur
	if p.srtt == 0 {
		p.srtt = rtt
		p.rttvar = rtt / 2
	} else {
		err := time.Duration(math.Abs(float64(rtt - p.srtt)))
		p.rttvar = time.Duration(0.75*float64(p.rttvar) + 0.25*float64(err))
		p.srtt = time.Duration(0.875*float64(p.srtt) + 0.125*float64(rtt))
	}
	p.currentTimeout = p.srtt + 4*p.rttvar
	// 确保超时时间不小于1秒
	if p.currentTimeout < time.Second {
		p.currentTimeout = time.Second
	}
	// 更新minRtt
	if p.minRtt == 0 || rtt < p.minRtt {
		p.minRtt = rtt
	}

	if p.debit > 0 {
		p.debit--

		if p.debit == 0 {
			switch p.state {
			case Lost:
				p.switchState(Normal)
			case Disconnected:
				p.switchState(Lost)
			}
		}
	}
}

func (p *Prober) switchState(to Event) {
	if p.state == to || (p.state == Disconnected && to != Normal) {
		return
	}

	oldState := p.state
	p.state = to

	switch to {
	case Lost:
		p.lost++
		p.debit = 10
	case Disconnected:
		p.debit = 1
	}

	fmt.Printf("Switch State %s: %s => %s Debit: %f\n", p.addr, oldState, p.state, p.debit)

	p.on(to)
}

func (p *Prober) start() {
	p.ticker = time.NewTicker(_probeInterval)
	defer p.ticker.Stop()

	p.sendProbePacket()

	for {
		select {
		case <-p.ctx.Done():
			return
		case pkt := <-p.in:
			p.recvProbePacket(pkt)
		case <-p.ticker.C:
			p.sendProbePacket()
		case <-p.deadline.C:
			if p.markTimeout() {
				p.switchState(Lost)
			}
		}
	}
}

func (p *Prober) Start(proberId uuid.UUID) {
	p.proberId = proberId
	go p.start()
}
