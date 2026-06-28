package recv

import "encoding/binary"

type ackTraceInfo struct {
	srcPort uint16
	dstPort uint16
	seq     uint32
	ack     uint32
}

type dataTraceInfo struct {
	srcPort    uint16
	dstPort    uint16
	seq        uint32
	ack        uint32
	payloadLen int
}

func ackTracePayload(packet []byte) (ackTraceInfo, bool) {
	info, ok := tcpTracePayload(packet)
	if !ok {
		return ackTraceInfo{}, false
	}
	if info.payloadLen != 0 || info.flags&0x10 == 0 || info.flags&0x07 != 0 {
		return ackTraceInfo{}, false
	}
	return ackTraceInfo{
		srcPort: info.srcPort,
		dstPort: info.dstPort,
		seq:     info.seq,
		ack:     info.ack,
	}, true
}

func dataTracePayload(packet []byte) (dataTraceInfo, bool) {
	info, ok := tcpTracePayload(packet)
	if !ok || info.payloadLen == 0 {
		return dataTraceInfo{}, false
	}
	return dataTraceInfo{
		srcPort:    info.srcPort,
		dstPort:    info.dstPort,
		seq:        info.seq,
		ack:        info.ack,
		payloadLen: info.payloadLen,
	}, true
}

type tcpTraceInfo struct {
	srcPort    uint16
	dstPort    uint16
	seq        uint32
	ack        uint32
	flags      byte
	payloadLen int
}

func tcpTracePayload(packet []byte) (tcpTraceInfo, bool) {
	if len(packet) < 20 {
		return tcpTraceInfo{}, false
	}
	if packet[0]>>4 != 4 {
		return tcpTraceInfo{}, false
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl+20 {
		return tcpTraceInfo{}, false
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen <= 0 || totalLen > len(packet) {
		totalLen = len(packet)
	}
	if totalLen < ihl+20 || packet[9] != 6 {
		return tcpTraceInfo{}, false
	}
	tcp := packet[ihl:totalLen]
	dataOffset := int(tcp[12]>>4) * 4
	if dataOffset < 20 || len(tcp) < dataOffset {
		return tcpTraceInfo{}, false
	}
	return tcpTraceInfo{
		srcPort:    binary.BigEndian.Uint16(tcp[0:2]),
		dstPort:    binary.BigEndian.Uint16(tcp[2:4]),
		seq:        binary.BigEndian.Uint32(tcp[4:8]),
		ack:        binary.BigEndian.Uint32(tcp[8:12]),
		flags:      tcp[13],
		payloadLen: len(tcp) - dataOffset,
	}, true
}
