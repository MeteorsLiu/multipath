package probe

import (
	"fmt"

	core "github.com/MeteorsLiu/multipath/internal/tunnel/probe/core"
)

func debugProbeEvent(event core.Event) string {
	return fmt.Sprintf("type=%s target=%d ping_id=%d time_ms=%d", debugProbeEventType(event.Type), event.Target, event.PingID, event.TimeMS)
}

func debugProbeEventType(eventType core.EventType) string {
	switch eventType {
	case core.EventTrack:
		return "TRACK"
	case core.EventUntrack:
		return "UNTRACK"
	case core.EventSendPing:
		return "SEND_PING"
	case core.EventPingFailed:
		return "PING_FAILED"
	case core.EventPongReceived:
		return "PONG_RECEIVED"
	case core.EventTargetLost:
		return "TARGET_LOST"
	case core.EventTargetRecovered:
		return "TARGET_RECOVERED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", eventType)
	}
}
