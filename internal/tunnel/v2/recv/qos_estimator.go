package recv

import (
	"math"
	"sync"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/eventlog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

const (
	defaultQoSTick          = time.Second
	defaultQoSGroupMature   = 1500 * time.Millisecond
	defaultFECLossAlpha     = 0.75
	defaultFECLossBeta      = 0.05
	defaultFECLateAlpha     = 0.20
	defaultFECLateBeta      = 0.05
	defaultRepairScaleAlpha = 0.25
	qosDecisionSamples      = 3

	qosRateGapLimited     = 0.10
	qosRateGapClear       = 0.03
	qosRepairLoadGapClear = 0.75
)

type qosConfig struct {
	SessionID uint64
	LaneID    uint8
	Tick      time.Duration
	AutoStart bool
}

type qosStatus struct {
	UDPLimited      bool
	TCPLimited      bool
	RepairCount     uint8
	UDPDeliveredBps uint32
	TCPDeliveredBps uint32
	primarySwitched bool
}

// qosEstimator owns all lane-local QoS and adaptive-FEC state. Recv submits
// packet and group facts; tick is the only path that commits LINK_STATUS state.
type qosEstimator struct {
	mu             sync.Mutex
	cfg            qosConfig
	emit           func(qosStatus)
	currentPrimary transport.Kind
	currentRole    qosRole

	udpTCPData   qosDirection
	udpTCPRepair qosDirection
	tcpUDPData   qosDirection
	tcpUDPRepair qosDirection

	groups        map[rxGroupKey]qosGroup
	lateSeen      *packetIDDedupe
	recoveredSeen *packetIDDedupe

	done     chan struct{}
	stopOnce sync.Once
	closed   bool
}

type qosRole uint8

const (
	qosRoleData qosRole = iota + 1
	qosRoleRepair
)

type qosDirection struct {
	dataKind   transport.Kind
	repairKind transport.Kind
	role       qosRole

	started    bool
	lastTickAt time.Time
	pending    qosPendingBytes

	dataLimited   bool
	repairLimited bool

	dataDelivered   qosDelivered
	repairDelivered qosDelivered
	dataDecision    qosDecisionWindow
	repairDecision  qosDecisionWindow
	repairLoad      qosDecisionWindow
	fecHealth       qosFECHealth

	repairScale            float64
	repairScaleInitialized bool
}

type qosPendingBytes struct {
	originalDataBytes uint64
	expectedBytes     uint64
	repairBytes       uint64
	lateDataBytes     uint64
}

type qosRates struct {
	originalDataBps uint32
	expectedBps     uint32
	repairBps       uint32
	lateDataBps     uint32
}

type qosDelivered struct {
	bps uint32
	at  time.Time
}

type qosFECHealth struct {
	lossRatio       float64
	lossInitialized bool
	lateRatio       float64
	lateInitialized bool
	dirty           bool
	repairCount     uint8
}

type qosDecisionWindow struct {
	values [qosDecisionSamples]float64
	count  int
	next   int
}

type qosGroup struct {
	dataKind         transport.Kind
	repairKind       transport.Kind
	repairCount      uint8
	directionTrusted bool
	repairTrusted    bool
}

func newQoSEstimator(cfg qosConfig, emit func(qosStatus)) *qosEstimator {
	if cfg.Tick <= 0 {
		cfg.Tick = defaultQoSTick
	}

	e := &qosEstimator{
		cfg:            cfg,
		emit:           emit,
		currentPrimary: transport.KindUDP,
		currentRole:    qosRoleData,
		groups:         make(map[rxGroupKey]qosGroup),
		lateSeen:       newPacketIDDedupe(0),
		recoveredSeen:  newPacketIDDedupe(0),
		done:           make(chan struct{}),
	}
	e.udpTCPData = newQoSDirection(transport.KindUDP, transport.KindTCP, qosRoleData)
	e.udpTCPRepair = newQoSDirection(transport.KindUDP, transport.KindTCP, qosRoleRepair)
	e.tcpUDPData = newQoSDirection(transport.KindTCP, transport.KindUDP, qosRoleData)
	e.tcpUDPRepair = newQoSDirection(transport.KindTCP, transport.KindUDP, qosRoleRepair)

	if cfg.AutoStart {
		go e.run()
	}
	return e
}

func (e *qosEstimator) observeOriginalData(dataKind transport.Kind, dataBytes int, at time.Time) {
	if dataBytes <= 0 || !knownQoSTransport(dataKind) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if direction := e.currentDirectionForLocked(dataKind, otherQoSTransport(dataKind)); direction != nil {
		direction.addOriginal(uint64(dataBytes), normalizeQoSTime(at))
	}
}

func newQoSDirection(dataKind, repairKind transport.Kind, role qosRole) qosDirection {
	return qosDirection{
		dataKind:   dataKind,
		repairKind: repairKind,
		role:       role,
		fecHealth:  qosFECHealth{repairCount: 1},
	}
}

func (e *qosEstimator) close() {
	e.stopOnce.Do(func() {
		close(e.done)
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
	})
}

func (e *qosEstimator) run() {
	ticker := time.NewTicker(e.cfg.Tick)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			e.emitStatuses(e.tick(now))
		case <-e.done:
			return
		}
	}
}

func (e *qosEstimator) observeLateData(packetID uint32, dataKind transport.Kind, dataBytes int, at time.Time) {
	if dataBytes <= 0 || !knownQoSTransport(dataKind) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if !e.recoveredSeen.seen(packetID) || !e.lateSeen.mark(packetID) {
		return
	}
	if direction := e.currentDirectionForLocked(dataKind, otherQoSTransport(dataKind)); direction != nil {
		direction.addLate(uint64(dataBytes), normalizeQoSTime(at))
	}
}

func (e *qosEstimator) observeRepairBytes(repairKind transport.Kind, repairBytes int, at time.Time) {
	if repairBytes <= 0 || !knownQoSTransport(repairKind) {
		return
	}

	dataKind := otherQoSTransport(repairKind)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	if direction := e.currentDirectionForLocked(dataKind, repairKind); direction != nil {
		direction.addRepair(uint64(repairBytes), normalizeQoSTime(at))
	}
}

func (e *qosEstimator) observeRepairGroup(group rxGroupKey, repairKind transport.Kind, repairCount uint8) {
	if !knownQoSTransport(repairKind) {
		return
	}
	if repairCount == 0 {
		repairCount = 1
	}

	dataKind := otherQoSTransport(repairKind)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}

	g := e.groups[group]
	if g.dataKind == 0 && g.repairKind == 0 {
		g = qosGroup{
			dataKind:         dataKind,
			repairKind:       repairKind,
			repairCount:      repairCount,
			directionTrusted: true,
			repairTrusted:    true,
		}
	} else {
		if g.dataKind != dataKind || g.repairKind != repairKind {
			g.directionTrusted = false
		}
		if g.repairCount != repairCount {
			g.repairTrusted = false
		}
	}
	e.groups[group] = g
}

func (e *qosEstimator) observeRecoveredData(packetID uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.recoveredSeen.mark(packetID)
}

func (e *qosEstimator) observeGroupDone(done rxGroupDone, at time.Time) {
	if done.dataExpected == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.finishGroupLocked(done, normalizeQoSTime(at))
}

func (e *qosEstimator) finishGroupLocked(done rxGroupDone, at time.Time) {
	group := e.groups[done.group]
	delete(e.groups, done.group)

	if !group.directionTrusted {
		return
	}
	direction := e.currentDirectionForLocked(group.dataKind, group.repairKind)
	if direction == nil {
		return
	}

	direction.addLossHealth(done.dataArrived, done.dataExpected, at)
	if done.expired {
		return
	}
	if repairScale, ok := group.repairScale(done); ok {
		direction.updateRepairScale(repairScale)
	}
	direction.addExpected(done.expectedBytes, at)
}

func (e *qosEstimator) tick(now time.Time) []qosStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}

	direction := e.currentDirectionLocked()
	now = normalizeQoSTime(now)
	rates, ok := direction.flush(now, e.cfg.Tick)
	if !ok {
		return nil
	}

	before := e.snapshotLocked()
	direction.dataDelivered = qosDelivered{bps: rates.originalDataBps, at: now}
	direction.repairDelivered = qosDelivered{bps: rates.repairBps, at: now}
	direction.addLateHealth(rates)
	e.recordRates(direction, rates)
	e.evaluateRatesLocked(direction, rates)
	repairChanged := direction.updateFECHealth()

	after := e.snapshotLocked()
	primarySwitched := e.switchPrimaryLocked(after)
	if primarySwitched {
		after = e.snapshotLocked()
	}
	if !primarySwitched && !shouldEmitQoSStatus(before, after, repairChanged) {
		return nil
	}
	after.primarySwitched = primarySwitched
	return []qosStatus{after}
}

func (e *qosEstimator) evaluateRatesLocked(direction *qosDirection, rates qosRates) {
	switch direction.role {
	case qosRoleData:
		if rates.expectedBps == 0 {
			return
		}
		actualDataBps := clampUint32(float64(rates.originalDataBps) + float64(rates.lateDataBps))
		gap := rateGapRatio(rates.expectedBps, actualDataBps)
		avgGap, ready := direction.dataDecision.add(gap)
		if !ready {
			return
		}
		if avgGap >= qosRateGapLimited {
			if !direction.dataLimited {
				e.recordEvent("limited_active", direction.dataKind)
			}
			direction.dataLimited = true
			return
		}
		if avgGap <= qosRateGapClear {
			if e.clearLimitedLocked(direction.dataKind) {
				e.recordEvent("limited_clear", direction.dataKind)
			}
		}
	case qosRoleRepair:
		if rates.expectedBps == 0 {
			return
		}
		if !direction.repairScaleInitialized {
			return
		}
		expectedRepairBps := clampUint32(float64(rates.expectedBps) * direction.repairScale)
		deliveryGap := rateGapRatio(expectedRepairBps, rates.repairBps)
		loadGap := rateGapRatio(rates.expectedBps, rates.repairBps)
		avgDeliveryGap, deliveryReady := direction.repairDecision.add(deliveryGap)
		avgLoadGap, loadReady := direction.repairLoad.add(loadGap)
		if !deliveryReady || !loadReady {
			return
		}
		if avgDeliveryGap >= qosRateGapLimited {
			if !direction.repairLimited {
				e.recordEvent("limited_active", direction.repairKind)
			}
			direction.repairLimited = true
			return
		}
		if avgDeliveryGap <= qosRateGapClear &&
			avgLoadGap <= qosRepairLoadGapClear &&
			e.clearLimitedLocked(direction.repairKind) {
			e.recordEvent("limited_clear", direction.repairKind)
		}
	}
}

func (e *qosEstimator) clearLimitedLocked(kind transport.Kind) bool {
	if kind == transport.KindUDP {
		return e.clearUDPLimitedLocked()
	}
	return e.clearTCPLimitedLocked()
}

func (e *qosEstimator) clearUDPLimitedLocked() bool {
	changed := false
	if e.udpTCPData.dataLimited {
		e.udpTCPData.dataLimited = false
		changed = true
	}
	if e.udpTCPRepair.dataLimited {
		e.udpTCPRepair.dataLimited = false
		changed = true
	}
	if e.tcpUDPData.repairLimited {
		e.tcpUDPData.repairLimited = false
		changed = true
	}
	if e.tcpUDPRepair.repairLimited {
		e.tcpUDPRepair.repairLimited = false
		changed = true
	}
	return changed
}

func (e *qosEstimator) clearTCPLimitedLocked() bool {
	changed := false
	if e.udpTCPData.repairLimited {
		e.udpTCPData.repairLimited = false
		changed = true
	}
	if e.udpTCPRepair.repairLimited {
		e.udpTCPRepair.repairLimited = false
		changed = true
	}
	if e.tcpUDPData.dataLimited {
		e.tcpUDPData.dataLimited = false
		changed = true
	}
	if e.tcpUDPRepair.dataLimited {
		e.tcpUDPRepair.dataLimited = false
		changed = true
	}
	return changed
}

func (e *qosEstimator) switchPrimaryLocked(status qosStatus) bool {
	nextPrimary := preferredPrimaryFromQoS(status)
	if nextPrimary == e.currentPrimary {
		return false
	}

	oldPrimary := e.currentPrimary
	nextRole := qosRoleData
	if e.limitedLocked(oldPrimary) {
		nextRole = qosRoleRepair
	}

	e.directionLocked(nextPrimary, oldPrimary, qosRoleData).resetRateState()
	if nextRole == qosRoleRepair {
		e.directionLocked(nextPrimary, oldPrimary, qosRoleRepair).resetRateState()
	}

	e.currentPrimary = nextPrimary
	e.currentRole = nextRole
	e.currentDirectionLocked().fecHealth = qosFECHealth{repairCount: 1}

	if debuglog.Enabled() {
		debuglog.Printf("recv/qos", "primary_switch session=%d lane=%d from=%s to=%s role=%s",
			e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(oldPrimary), kindMetricLabel(nextPrimary), qosRoleLabel(nextRole))
	}
	return true
}

func (e *qosEstimator) currentDirectionForLocked(dataKind, repairKind transport.Kind) *qosDirection {
	if dataKind != e.currentPrimary || repairKind != otherQoSTransport(e.currentPrimary) {
		return nil
	}
	return e.currentDirectionLocked()
}

func (e *qosEstimator) currentDirectionLocked() *qosDirection {
	if e.currentPrimary == transport.KindUDP {
		if e.currentRole == qosRoleRepair {
			return &e.udpTCPRepair
		}
		return &e.udpTCPData
	}
	if e.currentRole == qosRoleRepair {
		return &e.tcpUDPRepair
	}
	return &e.tcpUDPData
}

func (e *qosEstimator) directionLocked(dataKind, repairKind transport.Kind, role qosRole) *qosDirection {
	switch {
	case dataKind == transport.KindUDP && repairKind == transport.KindTCP && role == qosRoleData:
		return &e.udpTCPData
	case dataKind == transport.KindUDP && repairKind == transport.KindTCP && role == qosRoleRepair:
		return &e.udpTCPRepair
	case dataKind == transport.KindTCP && repairKind == transport.KindUDP && role == qosRoleData:
		return &e.tcpUDPData
	case dataKind == transport.KindTCP && repairKind == transport.KindUDP && role == qosRoleRepair:
		return &e.tcpUDPRepair
	default:
		return nil
	}
}

func (e *qosEstimator) limitedLocked(kind transport.Kind) bool {
	if kind == transport.KindUDP {
		return e.udpLimitedLocked()
	}
	return e.tcpLimitedLocked()
}

func (e *qosEstimator) udpLimitedLocked() bool {
	return e.udpTCPData.dataLimited ||
		e.udpTCPRepair.dataLimited ||
		e.tcpUDPData.repairLimited ||
		e.tcpUDPRepair.repairLimited
}

func (e *qosEstimator) tcpLimitedLocked() bool {
	return e.udpTCPData.repairLimited ||
		e.udpTCPRepair.repairLimited ||
		e.tcpUDPData.dataLimited ||
		e.tcpUDPRepair.dataLimited
}

func (e *qosEstimator) snapshotStatus() qosStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

func (e *qosEstimator) snapshotLocked() qosStatus {
	return qosStatus{
		UDPLimited:      e.udpLimitedLocked(),
		TCPLimited:      e.tcpLimitedLocked(),
		RepairCount:     e.currentDirectionLocked().fecHealth.repairCount,
		UDPDeliveredBps: e.udpDeliveredBpsLocked(),
		TCPDeliveredBps: e.tcpDeliveredBpsLocked(),
	}
}

func (e *qosEstimator) udpDeliveredBpsLocked() uint32 {
	latest := qosDelivered{}
	latest = newerDelivered(latest, e.udpTCPData.dataDelivered)
	latest = newerDelivered(latest, e.udpTCPRepair.dataDelivered)
	latest = newerDelivered(latest, e.tcpUDPData.repairDelivered)
	latest = newerDelivered(latest, e.tcpUDPRepair.repairDelivered)
	return latest.bps
}

func (e *qosEstimator) tcpDeliveredBpsLocked() uint32 {
	latest := qosDelivered{}
	latest = newerDelivered(latest, e.udpTCPData.repairDelivered)
	latest = newerDelivered(latest, e.udpTCPRepair.repairDelivered)
	latest = newerDelivered(latest, e.tcpUDPData.dataDelivered)
	latest = newerDelivered(latest, e.tcpUDPRepair.dataDelivered)
	return latest.bps
}

func (e *qosEstimator) emitStatuses(statuses []qosStatus) {
	if e.emit == nil {
		return
	}
	for _, status := range statuses {
		e.emit(status)
	}
}

func (d *qosDirection) addOriginal(bytes uint64, at time.Time) {
	d.start(at)
	d.pending.originalDataBytes += bytes
}

func (d *qosDirection) addExpected(bytes uint64, at time.Time) {
	if bytes == 0 {
		return
	}
	d.start(at)
	d.pending.expectedBytes += bytes
}

func (d *qosDirection) addRepair(bytes uint64, at time.Time) {
	d.start(at)
	d.pending.repairBytes += bytes
}

func (d *qosDirection) updateRepairScale(scale float64) {
	if scale <= 0 {
		return
	}
	if !d.repairScaleInitialized {
		d.repairScale = scale
		d.repairScaleInitialized = true
		return
	}
	d.repairScale = emaUpdate(d.repairScale, scale, defaultRepairScaleAlpha)
}

func (d *qosDirection) addLate(bytes uint64, at time.Time) {
	d.start(at)
	d.pending.lateDataBytes += bytes
}

func (d *qosDirection) addLossHealth(arrived, expected uint8, at time.Time) {
	d.start(at)
	if expected == 0 {
		return
	}
	lossRatio := 0.0
	if arrived < expected {
		lossRatio = float64(expected-arrived) / float64(expected)
	}
	if !d.fecHealth.lossInitialized {
		d.fecHealth.lossInitialized = true
		d.fecHealth.lossRatio = lossRatio
	} else if lossRatio > d.fecHealth.lossRatio {
		d.fecHealth.lossRatio = emaUpdate(d.fecHealth.lossRatio, lossRatio, defaultFECLossAlpha)
	} else {
		d.fecHealth.lossRatio = emaUpdate(d.fecHealth.lossRatio, lossRatio, defaultFECLossBeta)
	}
	d.fecHealth.dirty = true
}

func (d *qosDirection) addLateHealth(rates qosRates) {
	if rates.expectedBps == 0 {
		return
	}
	lateRatio := float64(rates.lateDataBps) / float64(rates.expectedBps)
	if !d.fecHealth.lateInitialized {
		d.fecHealth.lateInitialized = true
		d.fecHealth.lateRatio = lateRatio
	} else if lateRatio > d.fecHealth.lateRatio {
		d.fecHealth.lateRatio = emaUpdate(d.fecHealth.lateRatio, lateRatio, defaultFECLateAlpha)
	} else {
		d.fecHealth.lateRatio = emaUpdate(d.fecHealth.lateRatio, lateRatio, defaultFECLateBeta)
	}
	d.fecHealth.dirty = true
}

func (d *qosDirection) start(at time.Time) {
	if d.started {
		return
	}
	d.started = true
	d.lastTickAt = normalizeQoSTime(at)
}

func (d *qosDirection) flush(now time.Time, tick time.Duration) (qosRates, bool) {
	if !d.started {
		return qosRates{}, false
	}
	if now.Sub(d.lastTickAt) < tick {
		return qosRates{}, false
	}

	duration := now.Sub(d.lastTickAt)
	rates := qosRates{
		originalDataBps: bytesPerSecond(d.pending.originalDataBytes, duration),
		expectedBps:     bytesPerSecond(d.pending.expectedBytes, duration),
		repairBps:       bytesPerSecond(d.pending.repairBytes, duration),
		lateDataBps:     bytesPerSecond(d.pending.lateDataBytes, duration),
	}
	if debuglog.Enabled() {
		debuglog.Printf("recv/qos_rate", "tick role=%s data=%s repair=%s duration_ms=%d original_data=%d expected=%d repair=%d late_data=%d original_data_bps=%d expected_bps=%d repair_bps=%d late_data_bps=%d",
			qosRoleLabel(d.role), kindMetricLabel(d.dataKind), kindMetricLabel(d.repairKind),
			duration.Milliseconds(),
			d.pending.originalDataBytes, d.pending.expectedBytes, d.pending.repairBytes, d.pending.lateDataBytes,
			rates.originalDataBps, rates.expectedBps, rates.repairBps, rates.lateDataBps)
	}

	d.lastTickAt = now
	d.pending = qosPendingBytes{}
	return rates, true
}

func (d *qosDirection) updateFECHealth() bool {
	before := d.fecHealth.repairCount
	if !d.fecHealth.dirty {
		return false
	}
	d.fecHealth.dirty = false
	lossRepairCount := repairCountForLossRatio(d.fecHealth.lossRatio)
	lateRepairCount := repairCountForLateRatio(d.fecHealth.lateRatio)
	if lateRepairCount > lossRepairCount {
		d.fecHealth.repairCount = lateRepairCount
	} else {
		d.fecHealth.repairCount = lossRepairCount
	}
	return d.fecHealth.repairCount != before
}

func (d *qosDirection) resetRateState() {
	d.started = false
	d.lastTickAt = time.Time{}
	d.pending = qosPendingBytes{}
	d.repairScale = 0
	d.repairScaleInitialized = false
	d.clearDecisions()
}

func (d *qosDirection) clearDecisions() {
	d.dataDecision = qosDecisionWindow{}
	d.repairDecision = qosDecisionWindow{}
	d.repairLoad = qosDecisionWindow{}
}

func (w *qosDecisionWindow) add(value float64) (float64, bool) {
	w.values[w.next] = value
	w.next = (w.next + 1) % qosDecisionSamples
	if w.count < qosDecisionSamples {
		w.count++
	}
	if w.count < qosDecisionSamples {
		return 0, false
	}

	var sum float64
	for i := 0; i < w.count; i++ {
		sum += w.values[i]
	}
	return sum / float64(w.count), true
}

func newerDelivered(a, b qosDelivered) qosDelivered {
	if !b.at.IsZero() && (a.at.IsZero() || b.at.After(a.at)) {
		return b
	}
	return a
}

func preferredPrimaryFromQoS(status qosStatus) transport.Kind {
	switch {
	case status.UDPLimited && !status.TCPLimited:
		return transport.KindTCP
	case !status.UDPLimited && status.TCPLimited:
		return transport.KindUDP
	case status.UDPLimited && status.TCPLimited:
		if status.TCPDeliveredBps > status.UDPDeliveredBps {
			return transport.KindTCP
		}
		return transport.KindUDP
	default:
		return transport.KindUDP
	}
}

func knownQoSTransport(kind transport.Kind) bool {
	return kind == transport.KindUDP || kind == transport.KindTCP
}

func otherQoSTransport(kind transport.Kind) transport.Kind {
	if kind == transport.KindUDP {
		return transport.KindTCP
	}
	return transport.KindUDP
}

func normalizeQoSTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func bytesPerSecond(bytes uint64, duration time.Duration) uint32 {
	if bytes == 0 || duration <= 0 {
		return 0
	}
	return clampUint32(float64(bytes) * 8 * float64(time.Second) / float64(duration))
}

func rateGapRatio(expected, actual uint32) float64 {
	if expected == 0 || actual >= expected {
		return 0
	}
	return float64(expected-actual) / float64(expected)
}

func (g qosGroup) repairScale(done rxGroupDone) (float64, bool) {
	if !g.repairTrusted || g.repairCount == 0 || done.expectedBytes == 0 || done.maxSourceBytes == 0 {
		return 0, false
	}
	return float64(done.maxSourceBytes) / float64(done.expectedBytes) * float64(g.repairCount), true
}

func emaUpdate(current, sample, alpha float64) float64 {
	return current*(1-alpha) + sample*alpha
}

func repairCountForLossRatio(lossRatio float64) uint8 {
	if lossRatio <= 0 {
		return 1
	}
	repairCount := int(math.Ceil(lossRatio * maxFECSourceSpan))
	if repairCount < 1 {
		return 1
	}
	if repairCount > maxFECSourceSpan {
		return maxFECSourceSpan
	}
	return uint8(repairCount)
}

func repairCountForLateRatio(lateRatio float64) uint8 {
	switch {
	case lateRatio <= 0.50:
		return 1
	case lateRatio <= 0.75:
		return 2
	case lateRatio <= 1:
		return 3
	default:
		return maxFECSourceSpan
	}
}

func clampUint32(v float64) uint32 {
	if v <= 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func shouldEmitQoSStatus(before, after qosStatus, repairChanged bool) bool {
	if before.UDPLimited != after.UDPLimited || before.TCPLimited != after.TCPLimited {
		return true
	}
	if repairChanged || before.RepairCount != after.RepairCount {
		return true
	}
	if before.UDPLimited && before.TCPLimited && preferredPrimaryFromQoS(before) != preferredPrimaryFromQoS(after) {
		return true
	}
	return false
}

func qosRoleLabel(role qosRole) string {
	switch role {
	case qosRoleData:
		return "data"
	case qosRoleRepair:
		return "repair"
	default:
		return "unknown"
	}
}

func (e *qosEstimator) recordRates(direction *qosDirection, rates qosRates) {
	if e.cfg.SessionID == 0 {
		return
	}
	metrics.SetGauge(metrics.QoSDeliveredBps, float64(rates.originalDataBps),
		metrics.L("session", e.cfg.SessionID),
		metrics.L("lane", e.cfg.LaneID),
		metrics.LStr("leg", kindMetricLabel(direction.dataKind)),
	)
	metrics.SetGauge(metrics.QoSDeliveredBps, float64(rates.repairBps),
		metrics.L("session", e.cfg.SessionID),
		metrics.L("lane", e.cfg.LaneID),
		metrics.LStr("leg", kindMetricLabel(direction.repairKind)),
	)
}

func (e *qosEstimator) recordEvent(event string, kind transport.Kind) {
	if e.cfg.SessionID != 0 {
		metrics.IncCounter(metrics.QoSEventsTotal,
			metrics.LStr("event", event),
			metrics.L("session", e.cfg.SessionID),
			metrics.L("lane", e.cfg.LaneID),
			metrics.LStr("leg", kindMetricLabel(kind)),
		)
	}
	eventlog.Printf("qos_state", "event=%s session=%d lane=%d leg=%s",
		event, e.cfg.SessionID, e.cfg.LaneID, kindMetricLabel(kind))
}
