package send

import (
	"fmt"

	"github.com/MeteorsLiu/multipath/internal/transport"
)

func debugLeg(leg transport.LegRef) string {
	switch leg.Kind {
	case transport.KindUDP:
		remote := "<nil>"
		if leg.RemoteAddr != nil {
			remote = leg.RemoteAddr.String()
		}
		return fmt.Sprintf("udp endpoint=%s remote=%s", leg.EndpointID, remote)
	case transport.KindTCP:
		return fmt.Sprintf("tcp conn=%s", leg.ConnID)
	default:
		return fmt.Sprintf("kind=%d", leg.Kind)
	}
}
