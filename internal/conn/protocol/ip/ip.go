package ip

import (
	"encoding/binary"
	"fmt"
)

type Header []byte

func (i Header) Size() (uint16, error) {
	version := uint8(i[0]) >> 4

	switch version {
	case 4:
		if len(i) < 20 {
			return 0, fmt.Errorf("invalid ip4 header. Length %d less than 20", len(i))
		}
		return binary.BigEndian.Uint16(i[2:4]), nil
	case 6:
		if len(i) < 40 {
			return 0, fmt.Errorf("invalid ip6 header. Length %d less than 40", len(i))
		}
		return binary.BigEndian.Uint16(i[4:6]), nil
	}

	return 0, fmt.Errorf("unknown ip version")
}
