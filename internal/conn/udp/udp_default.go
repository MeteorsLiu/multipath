//go:build !linux

package udp

var srcControlSize int

// getSrcFromControl parses the control for PKTINFO and if found updates ep with
// the source information found.
func getSrcFromControl(control []byte, ep *StdNetEndpoint) {
}

// setSrcControl sets an IP{V6}_PKTINFO in control based on the source address
// and source ifindex found in ep. control's len will be set to 0 in the event
// that ep is a default value.
func setSrcControl(control *[]byte, ep *StdNetEndpoint) {
}
