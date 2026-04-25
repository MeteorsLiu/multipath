package send

import (
	"context"
	"errors"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

type helloRetry struct {
	awaiting   bool
	nonce      uint64
	leg        transport.LegRef
	payload    []byte
	caps       uint16
	fecProfile uint8
	startedMS  uint64
	lastSentMS uint64
}

func (r *helloRetry) start(nonce uint64, leg transport.LegRef, payload []byte, caps uint16, fecProfile uint8) {
	r.awaiting = true
	r.nonce = nonce
	r.leg = leg
	r.payload = append(r.payload[:0], payload...)
	r.caps = caps
	r.fecProfile = fecProfile
	r.startedMS = 0
	r.lastSentMS = 0
}

func (r *helloRetry) clear() {
	r.awaiting = false
	r.leg = transport.LegRef{}
	r.payload = nil
	r.startedMS = 0
	r.lastSentMS = 0
}

func (r *helloRetry) pending() bool {
	return r.awaiting && len(r.payload) != 0
}

func (r *helloRetry) matches(nonce uint64) bool {
	return r.awaiting && r.nonce == nonce
}

func (l *Send) retryPendingHELLO(ctx context.Context, nowMS uint64) error {
	for key, lane := range l.lanes {
		if lane == nil || !lane.helloRetry.pending() {
			continue
		}
		retry := &lane.helloRetry
		if retry.startedMS == 0 {
			retry.startedMS = nowMS
			debuglog.Printf("send/retry", "hello_retry_start session=%d lane=%d nonce=%d leg={%s}", key.sessionID, key.laneID, retry.nonce, debugLeg(retry.leg))
		}
		if l.probeTimeout > 0 && elapsedMS(nowMS, retry.startedMS) >= l.probeTimeout {
			leg := retry.leg
			retry.clear()
			debuglog.Printf("send/retry", "hello_retry_timeout session=%d lane=%d leg={%s}", key.sessionID, key.laneID, debugLeg(leg))
			switch leg.Kind {
			case transport.KindUDP:
				lane.udpReady = false
				l.trackProbeTarget(ctx, key.sessionID, key.laneID, leg)
				l.startFallbackDial(ctx, key, lane)
			case transport.KindTCP:
				lane.tcpReady = false
				if l.streamTransport != nil && leg.ConnID != "" {
					_ = l.streamTransport.Close(ctx, leg.ConnID)
				}
			}
			continue
		}
		if retry.lastSentMS != 0 && l.probeInterval > 0 && elapsedMS(nowMS, retry.lastSentMS) < l.probeInterval {
			continue
		}
		if err := l.writePayloadOnLeg(ctx, retry.leg, retry.payload); err != nil {
			if errors.Is(err, errLaneUnavailable) {
				debuglog.Printf("send/retry", "hello_retry_lane_unavailable session=%d lane=%d nonce=%d leg={%s}", key.sessionID, key.laneID, retry.nonce, debugLeg(retry.leg))
				continue
			}
			debuglog.Printf("send/retry", "hello_retry_send_err session=%d lane=%d nonce=%d leg={%s} err=%v", key.sessionID, key.laneID, retry.nonce, debugLeg(retry.leg), err)
			return err
		}
		retry.lastSentMS = nowMS
		debuglog.Printf("send/retry", "hello_retry_send session=%d lane=%d nonce=%d leg={%s} bytes=%d", key.sessionID, key.laneID, retry.nonce, debugLeg(retry.leg), len(retry.payload))
	}
	return nil
}

func elapsedMS(nowMS uint64, thenMS uint64) time.Duration {
	if nowMS <= thenMS {
		return 0
	}
	return time.Duration(nowMS-thenMS) * time.Millisecond
}
