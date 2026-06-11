package send

import (
	"context"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/metrics"
	"github.com/MeteorsLiu/multipath/internal/protocol"
)

const (
	maxFECSourceSpan         = 4
	defaultFECFlushAlpha     = 2
	defaultFECFlushMinMs     = 2
	defaultFECFlushMaxMs     = 30
	defaultFECFlushColdStart = 20
	fecFlushSendTimeout      = time.Second
)

func fecProfileEnabled(profile uint8) bool {
	return profile == protocol.FECProfileSLC4Plus1 || profile == protocol.FECProfileSLCVariablePlus1
}

func validFECProfile(profile uint8) bool {
	return profile == protocol.FECProfileOff || fecProfileEnabled(profile)
}

func (l *Send) fecCodecForSourceSpan(sourceSpan int) fecCodec {
	if sourceSpan <= 0 || sourceSpan > maxFECSourceSpan {
		return nil
	}
	if sourceSpan == maxFECSourceSpan && l.fecCodec != nil {
		return l.fecCodec
	}
	return l.fecCodecs[sourceSpan]
}

func (l *Send) computeFECFlushMs(sessionID uint64) uint32 {
	if l.fecFlushFixedMs > 0 {
		return l.fecFlushFixedMs
	}
	rtt, ok := l.sessionMaxRTTMs(sessionID)
	if !ok {
		rtt = l.fecFlushColdStartMs
	}
	alpha := l.fecFlushAlpha
	if alpha == 0 {
		alpha = defaultFECFlushAlpha
	}
	raw64 := uint64(rtt) * uint64(alpha)
	raw := uint32(raw64)
	if raw64 > uint64(^uint32(0)) {
		raw = ^uint32(0)
	}
	return clampUint32(raw, l.fecFlushMinMs, l.fecFlushMaxMs)
}

func (l *Send) armFECFlushTimer(sessionID uint64, lane *laneRuntime) {
	if lane == nil {
		return
	}
	flushMS := l.computeFECFlushMs(sessionID)
	if flushMS == 0 {
		flushMS = 1
	}
	d := time.Duration(flushMS) * time.Millisecond

	lane.fecMu.Lock()
	defer lane.fecMu.Unlock()
	if lane.txWindow == nil || len(lane.txWindow.pending) == 0 {
		return
	}
	if lane.fecFlushTimer == nil {
		lane.fecFlushTimer = time.AfterFunc(d, func() {
			l.handleFECFlush(sessionID, lane)
		})
	} else {
		lane.fecFlushTimer.Reset(d)
	}
	lane.fecFlushArmed = true
	if debuglog.Enabled() {
		debuglog.Printf("send", "fec_flush_arm session=%d lane=%d flush_ms=%d pending=%d", sessionID, lane.id, flushMS, len(lane.txWindow.pending))
	}
}

func (l *Send) handleFECFlush(sessionID uint64, lane *laneRuntime) {
	if lane == nil || uint8(l.fecProfile.Load()) != protocol.FECProfileSLCVariablePlus1 {
		return
	}
	// Ignore the timer if the lane was closed or replaced since it was armed.
	if l.getLane(laneKey{sessionID: sessionID, laneID: lane.id}) != lane {
		return
	}

	lane.fecMu.Lock()
	lane.fecFlushArmed = false
	var group txRepairGroup
	var ready bool
	if lane.txWindow != nil {
		group, ready = lane.txWindow.flush()
	}
	lane.fecMu.Unlock()
	if !ready {
		return
	}

	if debuglog.Enabled() {
		debuglog.Printf("send", "fec_flush_fire session=%d lane=%d base_packet_id=%d source_span=%d", sessionID, lane.id, group.basePacketID, group.sourceSpan)
	}
	metrics.IncCounter(metrics.FECEventsTotal,
		metrics.L("event", "repair_flush"),
		metrics.L("session", sessionID),
		metrics.L("source_span", group.sourceSpan),
	)
	metrics.IncCounter(metrics.FECFlushTotal,
		metrics.L("session", sessionID),
		metrics.L("source_span", group.sourceSpan),
	)
	ctx, cancel := context.WithTimeout(context.Background(), fecFlushSendTimeout)
	defer cancel()
	l.maybeSendRepair(ctx, sessionID, lane, group)
}

func clampUint32(v, min, max uint32) uint32 {
	if min > 0 && v < min {
		return min
	}
	if max > 0 && v > max {
		return max
	}
	return v
}
