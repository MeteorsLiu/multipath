package send

import (
	"context"
	"errors"
	"time"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	sessionpkg "github.com/MeteorsLiu/multipath/internal/session"
	"github.com/MeteorsLiu/multipath/internal/transport"
)

type helloRoute struct {
	hello        *sessionpkg.Hello
	leg          transport.LegRef
	payload      []byte
	firstRetryMS uint64
	lastRetryMS  uint64
}

func (r *helloRoute) set(hello *sessionpkg.Hello, leg transport.LegRef, payload []byte) {
	r.hello = hello
	r.leg = leg
	r.payload = append(r.payload[:0], payload...)
	r.firstRetryMS = 0
	r.lastRetryMS = 0
}

func (r helloRoute) valid() bool {
	return r.hello != nil && len(r.payload) != 0
}

func (r helloRoute) nonce() uint64 {
	var nonce uint64
	if r.hello == nil {
		return 0
	}
	_ = r.hello.Do(func(v sessionpkg.View) error {
		nonce = v.Nonce()
		return nil
	})
	return nonce
}

func (r helloRoute) matches(nonce uint64) bool {
	return r.hello != nil && r.nonce() == nonce
}

func (l *Send) retryOpenHELLO(ctx context.Context, nowMS uint64) error {
	for key, route := range l.helloRoutes {
		if !route.valid() {
			delete(l.helloRoutes, key)
			continue
		}
		nonce := route.nonce()
		if route.firstRetryMS == 0 {
			route.firstRetryMS = nowMS
			l.helloRoutes[key] = route
			debuglog.Printf("send/retry", "hello_retry_start session=%d lane=%d nonce=%d leg={%s}", key.sessionID, key.laneID, nonce, debugLeg(route.leg))
		}
		if l.probeTimeout > 0 && elapsedMS(nowMS, route.firstRetryMS) >= l.probeTimeout {
			l.cancelHelloRoute(key)
			lane := l.lanes[key]
			if lane == nil {
				debuglog.Printf("send/retry", "hello_retry_timeout missing_lane session=%d lane=%d nonce=%d", key.sessionID, key.laneID, nonce)
				continue
			}
			leg := route.leg
			debuglog.Printf("send/retry", "hello_retry_timeout session=%d lane=%d nonce=%d leg={%s}", key.sessionID, key.laneID, nonce, debugLeg(leg))
			switch leg.Kind {
			case transport.KindUDP:
				lane.udpReady = false
				l.markRunnableLanesDirty(key.sessionID)
				l.trackProbeTarget(ctx, key.sessionID, key.laneID, leg)
				l.startFallbackDial(ctx, key, lane)
			case transport.KindTCP:
				lane.tcpReady = false
				l.markRunnableLanesDirty(key.sessionID)
				if l.streamTransport != nil && leg.ConnID != "" {
					_ = l.streamTransport.Close(ctx, leg.ConnID)
				}
			}
			continue
		}
		if route.lastRetryMS != 0 && l.probeInterval > 0 && elapsedMS(nowMS, route.lastRetryMS) < l.probeInterval {
			continue
		}
		sent, expired, err := route.hello.Retry(nowMS, func(sessionpkg.View) error {
			return l.writePayloadOnLeg(ctx, route.leg, route.payload)
		})
		if err != nil {
			if errors.Is(err, errLaneUnavailable) {
				debuglog.Printf("send/retry", "hello_retry_lane_unavailable session=%d lane=%d nonce=%d leg={%s}", key.sessionID, key.laneID, nonce, debugLeg(route.leg))
				continue
			}
			debuglog.Printf("send/retry", "hello_retry_send_err session=%d lane=%d nonce=%d leg={%s} err=%v", key.sessionID, key.laneID, nonce, debugLeg(route.leg), err)
			return err
		}
		if expired {
			delete(l.helloRoutes, key)
			debuglog.Printf("send/retry", "hello_retry_expired session=%d lane=%d nonce=%d", key.sessionID, key.laneID, nonce)
			continue
		}
		if !sent {
			continue
		}
		route.lastRetryMS = nowMS
		l.helloRoutes[key] = route
		debuglog.Printf("send/retry", "hello_retry_send session=%d lane=%d nonce=%d leg={%s} bytes=%d", key.sessionID, key.laneID, nonce, debugLeg(route.leg), len(route.payload))
	}
	return nil
}

func elapsedMS(nowMS uint64, thenMS uint64) time.Duration {
	if nowMS <= thenMS {
		return 0
	}
	return time.Duration(nowMS-thenMS) * time.Millisecond
}
