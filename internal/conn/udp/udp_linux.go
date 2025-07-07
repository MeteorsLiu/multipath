//go:build linux

package udp

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

var srcControlSize = unix.CmsgSpace(unix.SizeofInet6Pktinfo)

// getSrcFromControl parses the control for PKTINFO and if found updates ep with
// the source information found.
func getSrcFromControl(control []byte, ep *StdNetEndpoint) {
	ep.ClearSrc()

	var (
		hdr  unix.Cmsghdr
		data []byte
		rem  []byte = control
		err  error
	)

	for len(rem) > unix.SizeofCmsghdr {
		hdr, data, rem, err = unix.ParseOneSocketControlMessage(rem)
		if err != nil {
			return
		}

		if hdr.Level == unix.IPPROTO_IP &&
			hdr.Type == unix.IP_PKTINFO {

			if ep.src == nil || cap(ep.src) < unix.CmsgSpace(unix.SizeofInet4Pktinfo) {
				ep.src = make([]byte, 0, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
			}
			ep.src = ep.src[:unix.CmsgSpace(unix.SizeofInet4Pktinfo)]

			hdrBuf := unsafe.Slice((*byte)(unsafe.Pointer(&hdr)), unix.SizeofCmsghdr)
			copy(ep.src, hdrBuf)
			copy(ep.src[unix.CmsgLen(0):], data)
			return
		}

		if hdr.Level == unix.IPPROTO_IPV6 &&
			hdr.Type == unix.IPV6_PKTINFO {

			if ep.src == nil || cap(ep.src) < unix.CmsgSpace(unix.SizeofInet6Pktinfo) {
				ep.src = make([]byte, 0, unix.CmsgSpace(unix.SizeofInet6Pktinfo))
			}

			ep.src = ep.src[:unix.CmsgSpace(unix.SizeofInet6Pktinfo)]

			hdrBuf := unsafe.Slice((*byte)(unsafe.Pointer(&hdr)), unix.SizeofCmsghdr)
			copy(ep.src, hdrBuf)
			copy(ep.src[unix.CmsgLen(0):], data)
			return
		}
	}
}

// setSrcControl sets an IP{V6}_PKTINFO in control based on the source address
// and source ifindex found in ep. control's len will be set to 0 in the event
// that ep is a default value.
func setSrcControl(control *[]byte, ep *StdNetEndpoint) {
	if cap(*control) < len(ep.src) {
		return
	}
	*control = (*control)[:0]
	*control = append(*control, ep.src...)
}
