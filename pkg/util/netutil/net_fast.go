package netutil

import "net"

// ParseIPFast extracts source, destination IP and protocol from a raw IP packet without allocations.
func ParseIPFast(packet []byte) (src, dst net.IP, protocol int, err error) {
	if len(packet) < 1 {
		return nil, nil, -1, errInvalidPacket
	}
	version := packet[0] >> 4
	switch version {
	case 4:
		if len(packet) < 20 {
			return nil, nil, -1, errInvalidPacket
		}
		// IPv4: protocol at byte 9, src at 12-16, dst at 16-20
		return net.IP(packet[12:16]), net.IP(packet[16:20]), int(packet[9]), nil
	case 6:
		if len(packet) < 40 {
			return nil, nil, -1, errInvalidPacket
		}
		// IPv6: next header at byte 6, src at 8-24, dst at 24-40
		return net.IP(packet[8:24]), net.IP(packet[24:40]), int(packet[6]), nil
	default:
		return nil, nil, -1, errInvalidPacket
	}
}

var errInvalidPacket = &invalidPacketError{}

type invalidPacketError struct{}

func (e *invalidPacketError) Error() string { return "invalid IP packet" }
