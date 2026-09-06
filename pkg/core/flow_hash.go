package core

import (
	"encoding/binary"
	"net"
)

// flowKey holds the L4 fields used for five-tuple slot selection. The destination IP is
// not stored here — the caller already has it as packet.dst and passes it to flowHash —
// so this struct only carries what parseFiveTupleInline must dig out of the L4 header.
//
// hasPorts is false for packets that carry no usable L4 ports: IP fragments (only the
// first fragment has the L4 header), ICMP, and protocols/headers we do not parse. In
// that case flowHash falls back to dst-IP hashing, which keeps every fragment of one
// datagram (they share the dst IP) on the same slot and preserves legacy behavior.
type flowKey struct {
	proto    uint8
	srcPort  uint16
	dstPort  uint16
	hasPorts bool
}

// parseFiveTupleInline extracts the L4 protocol and ports from a raw IP packet (the bytes
// at packet.data[tunReserve:], i.e. starting at the IP version nibble). It is zero-alloc:
// it only reads a handful of bytes and returns a value type. The IP packet is assumed to
// have already passed ParseIPFast (so the version/length are sane), but every offset is
// still bounds-checked defensively — a malformed packet just yields hasPorts=false and is
// routed by dst IP.
//
// Only TCP/UDP/SCTP carry the 16-bit src/dst ports at offsets 0/2 of the L4 header.
// IPv4 fragments (MF set or fragment-offset != 0) and any other protocol fall back.
func parseFiveTupleInline(ipData []byte) flowKey {
	if len(ipData) < 1 {
		return flowKey{}
	}
	switch ipData[0] >> 4 {
	case 4:
		if len(ipData) < 20 {
			return flowKey{}
		}
		// IPv4 fragment: only the first fragment (offset 0, MF possibly set) carries the
		// L4 header; later fragments do not. Route the whole datagram by dst IP so every
		// fragment lands on the same slot. flagsFrag = [3-bit flags][13-bit frag offset];
		// bit 13 (0x2000) is MF, the low 13 bits are the offset.
		flagsFrag := binary.BigEndian.Uint16(ipData[6:8])
		if flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0 {
			return flowKey{}
		}
		proto := ipData[9]
		ihl := int(ipData[0]&0x0f) * 4
		if ihl < 20 {
			return flowKey{}
		}
		return l4Ports(proto, ipData, ihl)
	case 6:
		if len(ipData) < 40 {
			return flowKey{}
		}
		// Extension headers are not walked: if the next header is not a port-bearing L4
		// protocol we fall back to dst-IP hashing. Extension-header packets are rare in
		// practice (the common case is bare TCP/UDP), so this is an accepted limitation.
		return l4Ports(ipData[6], ipData, 40)
	default:
		return flowKey{}
	}
}

// l4Ports reads the src/dst ports for port-bearing protocols, given the L4 header offset.
// Returns hasPorts=false (dst-IP fallback) for non-port protocols or a truncated header.
func l4Ports(proto uint8, ipData []byte, l4Off int) flowKey {
	switch proto {
	case 6, 17, 132: // TCP, UDP, SCTP
		if len(ipData) < l4Off+4 {
			return flowKey{}
		}
		return flowKey{
			proto:    proto,
			srcPort:  binary.BigEndian.Uint16(ipData[l4Off : l4Off+2]),
			dstPort:  binary.BigEndian.Uint16(ipData[l4Off+2 : l4Off+4]),
			hasPorts: true,
		}
	default:
		return flowKey{}
	}
}

// flowHash returns a consistent slot index for an L4 flow, mixing proto, dst IP, and both
// ports (FNV-1a, same style as ipHash). Spreading by the full five-tuple keeps many flows
// to one hot dst IP balanced across the pool, instead of pinning them all to one slot.
//
// The source IP is deliberately omitted: a client has a single TUN IP, so it adds nothing
// to the distribution (the analog of IPVS source-hashing using only the discriminating
// fields). When the packet has no usable ports (fragment/ICMP/unparsed), it falls back to
// ipHash(dstIP) so a flow's fragments stay together and legacy behavior is preserved.
//
// Determinism is a hard requirement: every packet of one flow MUST map to the same slot,
// because the server runs an independent gvisor stack per pool connection — splitting a
// flow across slots would land its packets on different stacks and corrupt TCP state.
func flowHash(key flowKey, dstIP net.IP, slots int) int {
	if !key.hasPorts {
		return ipHash(dstIP, slots)
	}
	var h uint32 = 2166136261
	h ^= uint32(key.proto)
	h *= 16777619
	for _, b := range dstIP {
		h ^= uint32(b)
		h *= 16777619
	}
	h ^= uint32(key.srcPort >> 8)
	h *= 16777619
	h ^= uint32(key.srcPort & 0xff)
	h *= 16777619
	h ^= uint32(key.dstPort >> 8)
	h *= 16777619
	h ^= uint32(key.dstPort & 0xff)
	h *= 16777619
	return int(h % uint32(slots))
}

// ipHash returns a consistent slot index for an IP address.
// Uses FNV-1a-like hash for fast, well-distributed mapping.
func ipHash(ip net.IP, slots int) int {
	var h uint32 = 2166136261
	for _, b := range ip {
		h ^= uint32(b)
		h *= 16777619
	}
	return int(h % uint32(slots))
}
