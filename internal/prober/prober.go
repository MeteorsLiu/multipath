package prober

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/vary"
)

const (
	_defaultTimeout    = 15
	_defaultTimeoutDur = _defaultTimeout * time.Second
)

var packetPool = sync.Pool{
	New: func() any {
		return &Packet{Nonce: make([]byte, 8)}
	},
}

type Event int

const (
	Normal Event = iota
	Disconnected
	Lost
)

type Packet struct {
	Nonce []byte
}

func NewProbePacket(buf []byte) *Packet {
	packet := packetPool.Get().(*Packet)

	if buf != nil {
		copy(packet.Nonce, buf)
	}

	return packet
}

func (p *Packet) Release() {
	packetPool.Put(p)
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
	on    func(Event)
	state Event

	in  chan *Packet
	out chan *Packet

	avg            *vary.Vary
	minRtt         float64
	lost           float64
	debit          float64
	deadline       *time.Timer
	reschedule     *time.Timer
	currentTimeout time.Duration

	packetMap        map[uint64]*packetInfo
	lastMaxStartTime int64
}

func New(ctx context.Context, on func(Event)) *Prober {
	p := &Prober{
		on:     on,
		avg:    vary.NewVary(),
		in:     make(chan *Packet, 128),
		out:    make(chan *Packet, 128),
		minRtt: math.MaxFloat64,

		deadline:   new(time.Timer),
		reschedule: new(time.Timer),
		packetMap:  make(map[uint64]*packetInfo),
	}
	go p.run(ctx)
	return p
}

func (p *Prober) In() chan<- *Packet {
	return p.in
}

func (p *Prober) Out() <-chan *Packet {
	return p.out
}

func (p *Prober) sendProbePacket() {
	packet := NewProbePacket(nil)
	io.ReadFull(rand.Reader, packet.Nonce)

	nonce := binary.LittleEndian.Uint64(packet.Nonce)

	p.packetMap[nonce] = &packetInfo{startTime: time.Now()}

	p.out <- packet

	if p.avg.IsZero() {
		p.deadline = time.NewTimer(_defaultTimeoutDur)
		return
	}

	uclRtt := math.Pow(math.E, p.avg.UCL(3)) + p.minRtt
	p.currentTimeout = time.Duration(uclRtt) * time.Microsecond

	if p.deadline.C == nil {
		p.deadline = time.NewTimer(p.currentTimeout)
		return
	}
	p.deadline.Reset(p.currentTimeout)
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

	for nonce := range p.packetMap {
		pkt := p.packetMap[nonce]

		elapsed := time.Since(pkt.startTime)

		// if one of packet has no reply over 15 seconds, mark it disconnected.
		if elapsed >= _defaultTimeoutDur {
			p.switchState(Disconnected)
			// we need to send a probe packet right now if we confirm it's disconnected.
			isTimeout = true
		}

		if !pkt.isTimeout && elapsed.Microseconds() >= p.currentTimeout.Microseconds() {
			pkt.isTimeout = true
			isTimeout = true
		}

		if needGC {
			// only GC old packets best-effort.
			heap.Push(&timeHeap, &gcPacket{
				nonce:   nonce,
				elasped: elapsed,
			})
			// if we GC the maxmium timestamp, it may not trigger the 15s alive check
			// need to record it, reset it when normal packet arrived
			//
			// unit: second, which is enough
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

func (p *Prober) recvProbePacket(packet *Packet) {
	defer packet.Release()

	nonce := binary.LittleEndian.Uint64(packet.Nonce)

	info, ok := p.packetMap[nonce]
	if !ok {
		// has been GC or unknown
		return
	}
	defer delete(p.packetMap, nonce)

	elapsedTimeUs := time.Since(info.startTime).Microseconds()

	// check twice, this aims to avoid the case receiving probe packet and reaching deadline concurrently.
	isTimeout := info.isTimeout ||
		elapsedTimeUs >= p.currentTimeout.Microseconds()

	if isTimeout {
		if p.debit > 0 {
			p.debit = 10
		}
		return
	}
	p.lastMaxStartTime = 0
	// clear timeout
	p.deadline.Stop()

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

	elapsedTime := float64(elapsedTimeUs)

	p.minRtt = min(elapsedTime, p.minRtt)

	// compress rtt
	compressedRtt := math.Log(elapsedTime)

	p.avg.Calculate(compressedRtt)

	uclRtt := math.Pow(math.E, p.avg.UCL(3)) + p.minRtt

	if p.reschedule.C == nil {
		p.reschedule = time.NewTimer(time.Duration(uclRtt) * time.Microsecond)
		return
	}
	p.reschedule.Reset(time.Duration(uclRtt) * time.Microsecond)
}

func (p *Prober) switchState(to Event) {
	if p.state == to || (p.state == Disconnected && to != Normal) {
		return
	}

	p.state = to

	switch to {
	case Lost:
		p.lost++
		p.debit = 10
	case Disconnected:
		p.debit = 1
	}

	p.on(to)
}

// State:
// Normal <-> Lost:
// Normal -> Lost (When one packet reaches the deadline)
// Lost -> Normal (When 10 packets meets estimated RTT requirement)
//
// Normal <-> Lost <-> Disconnect <- Normal
// Normal -> Disconnect (When one packet reaches the maximum deadline, 15s)
// Lost -> Disconnect (When one packet reaches the maximum deadline, 15s)
// Disconnect -> Lost (When 1 packets meets estimated RTT requirement)
// Lost -> Normal: See Normal <-> Lost
func (p *Prober) run(ctx context.Context) {
	p.sendProbePacket()

	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-p.in:
			p.recvProbePacket(pkt)
		case <-p.reschedule.C:
			p.sendProbePacket()
		case <-p.deadline.C:
			// make sure we're really in timeout
			if p.markTimeout() {
				p.switchState(Lost)
				p.sendProbePacket()
			}
		}
	}
}
