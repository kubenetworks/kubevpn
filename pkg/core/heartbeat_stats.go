package core

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

const (
	probeMagic       = "KVPNHB01"
	probePayloadSize = len(probeMagic) + 16 + 8 + 8
	maxPendingProbes = 128
	probeLifetime    = 30 * time.Second
)

func isHeartbeatEchoReply(ipPacket []byte) bool {
	return netutil.IsICMPEchoReplyFrom(ipPacket, config.RouterIP) ||
		netutil.IsICMPEchoReplyFrom(ipPacket, config.RouterIP6)
}

// HeartbeatSnapshot is a value-only, bounded diagnostic snapshot. Slot indices
// 0..3 are data slots; index ConnPoolSize is control. Counters are cumulative,
// not health verdicts: reverse traffic need not arrive on the request's slot.
type HeartbeatSnapshot struct {
	Generation                                uint64
	LastReply                                 time.Time
	LastRTT                                   time.Duration
	Pending                                   int
	Sent, Replies, Rejected, Expired, Evicted uint64
	Slots                                     [ConnPoolSize + 1]SlotSnapshot
}

// SlotSnapshot records activity, not inferred per-slot health. Bytes exclude the
// two-byte framing header and include the packet type prefix. Reasons are bounded.
type SlotSnapshot struct {
	Generation                                       uint64
	Connected                                        bool
	LastRead, LastWrite, LastReconnect               time.Time
	ReadBytes, WriteBytes, ReadPackets, WritePackets uint64
	Reconnects                                       uint64
	LastReconnectReason                              string
	ProbesSent, ProbeReplies, RepliesArrived         uint64
	LastRTT                                          time.Duration
}

type pendingProbe struct {
	sent        time.Time
	slot        int
	identity    [4]byte
	destination string
	written     bool
	reply       time.Time
	arrival     int
}

// HeartbeatStats correlates gateway echoes with probes actually written by this
// session. Its zero value is usable (generation 0); nil receivers are no-ops.
// Do not copy it after use. The clock seam is set only before use in tests.
type HeartbeatStats struct {
	// Lock order: mu (probe state), then individual slotMu. Packet activity
	// takes only its own slot lock and never contends on pending probes.
	mu          sync.Mutex
	slotMu      [ConnPoolSize + 1]sync.Mutex
	nonce       [16]byte
	initialized bool
	counter     uint64
	pending     map[uint64]pendingProbe
	wake        chan struct{}
	now         func() time.Time
	snapshot    HeartbeatSnapshot
}

func (h *HeartbeatStats) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// BeginGeneration fences old probes and resets liveness and per-generation slot
// diagnostics, retaining cumulative counters. Then retire the underlying transport
// connections; the existing slots capture this generation on their next dial.
// It does not reconnect transports or send a probe by itself.
func (h *HeartbeatStats) BeginGeneration() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshot.Generation++
	h.snapshot.LastReply = time.Time{}
	h.snapshot.LastRTT = 0
	for i := range h.snapshot.Slots {
		h.slotMu[i].Lock()
		s := &h.snapshot.Slots[i]
		s.Generation = h.snapshot.Generation
		s.Connected = false
		s.LastRead = time.Time{}
		s.LastWrite = time.Time{}
		s.LastReconnect = time.Time{}
		s.LastReconnectReason = ""
		s.LastRTT = 0
		h.slotMu[i].Unlock()
	}
	clear(h.pending)
	return h.snapshot.Generation
}

// ClearPending requires fresh evidence without retiring the transport. Late echoes
// and in-flight write completions cannot renew health once their pending entry is
// removed. Session identity, generation, counters and slot diagnostics are retained.
func (h *HeartbeatStats) ClearPending() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.pending)
	h.snapshot.LastReply = time.Time{}
	h.snapshot.LastRTT = 0
}

func (h *HeartbeatStats) Generation() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot.Generation
}

func (h *HeartbeatStats) probeRequests() <-chan struct{} {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.wake == nil {
		h.wake = make(chan struct{}, 1)
	}
	return h.wake
}

// RequestProbe coalesces nonblocking wake requests for the heartbeat loop. It is
// safe before loop startup and after cancellation; no goroutine is created.
func (h *HeartbeatStats) RequestProbe() {
	if h == nil {
		return
	}
	h.probeRequests()
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// MarkReply is retained for legacy test callers. Production must correlate replies.
func (h *HeartbeatStats) MarkReply() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshot.LastReply = h.clock()
}

func (h *HeartbeatStats) LastReply() time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot.LastReply
}

func (h *HeartbeatStats) expire(now time.Time) {
	for id, p := range h.pending {
		if now.Sub(p.sent) >= probeLifetime {
			delete(h.pending, id)
			h.snapshot.Expired++
		}
	}
}

func (h *HeartbeatStats) Snapshot() HeartbeatSnapshot {
	if h == nil {
		return HeartbeatSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire(h.clock())
	for i := range h.slotMu {
		h.slotMu[i].Lock()
	}
	s := h.snapshot
	for i := len(h.slotMu) - 1; i >= 0; i-- {
		h.slotMu[i].Unlock()
	}
	s.Pending = len(h.pending)
	return s
}

// echoOffset accepts only complete, unfragmented IP/ICMP echo packets. Our
// generated IPv6 probes have no extension headers. No allocation on business data.
func echoOffset(ip []byte) int {
	if len(ip) >= 28 && ip[0]>>4 == 4 {
		n := int(ip[0]&15) * 4
		if n >= 20 && len(ip) >= n+8 && int(binary.BigEndian.Uint16(ip[2:4])) == len(ip) && ip[9] == 1 && binary.BigEndian.Uint16(ip[6:8])&0x3fff == 0 && ip[n+1] == 0 {
			return n
		}
	}
	if len(ip) >= 48 && ip[0]>>4 == 6 && ip[6] == 58 && int(binary.BigEndian.Uint16(ip[4:6]))+40 == len(ip) && ip[41] == 0 {
		return 40
	}
	return -1
}

// prepareProbe is called only at a slot's actual write, never when queueing.
// A provisional entry allows a fast reply to race Write's return without counting
// an unsuccessful write as healthy. finishProbe commits or removes that entry.
func (h *HeartbeatStats) prepareProbe(ip []byte, generation uint64, slot int) ([]byte, uint64) {
	if h == nil {
		return ip, 0
	}
	off := echoOffset(ip)
	if off < 0 || (ip[0]>>4 == 4 && ip[off] != 8) || (ip[0]>>4 == 6 && ip[off] != 128) {
		return ip, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation != h.snapshot.Generation {
		return nil, 0
	}
	if !h.initialized {
		_, _ = rand.Read(h.nonce[:])
		h.initialized = true
		h.pending = make(map[uint64]pendingProbe)
	}
	now := h.clock()
	h.expire(now)
	if len(h.pending) >= maxPendingProbes {
		var oldest uint64
		for id := range h.pending {
			if oldest == 0 || id < oldest {
				oldest = id
			}
		}
		delete(h.pending, oldest)
		h.snapshot.Evicted++
	}
	h.counter++
	out := make([]byte, off+8+probePayloadSize)
	copy(out, ip[:off+8])
	payload := out[off+8:]
	copy(payload, probeMagic)
	copy(payload[len(probeMagic):], h.nonce[:])
	binary.BigEndian.PutUint64(payload[24:32], generation)
	binary.BigEndian.PutUint64(payload[32:40], h.counter)
	var destination string
	if off == 40 && ip[0]>>4 == 6 {
		v6 := header.IPv6(out)
		v6.SetPayloadLength(uint16(len(out) - 40))
		icmp := header.ICMPv6(out[off:])
		icmp.SetChecksum(0)
		icmp.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{Header: icmp, Src: v6.SourceAddress(), Dst: v6.DestinationAddress()}))
		destination = string(ip[8:24])
	} else {
		binary.BigEndian.PutUint16(out[2:4], uint16(len(out)))
		v4 := header.IPv4(out)
		v4.SetChecksum(0)
		v4.SetChecksum(^v4.CalculateChecksum())
		icmp := header.ICMPv4(out[off:])
		icmp.SetChecksum(0)
		icmp.SetChecksum(header.ICMPv4Checksum(icmp, 0))
		destination = string(ip[12:16])
	}
	h.pending[h.counter] = pendingProbe{sent: now, slot: slot, identity: [4]byte(out[off+4 : off+8]), destination: destination}
	return out, h.counter
}

func (h *HeartbeatStats) accept(id uint64, p pendingProbe) {
	delete(h.pending, id)
	h.snapshot.Replies++
	rtt := p.reply.Sub(p.sent)
	if p.reply.After(h.snapshot.LastReply) {
		h.snapshot.LastReply = p.reply
		h.snapshot.LastRTT = rtt
	}
	if p.slot >= 0 && p.slot <= ConnPoolSize {
		h.slotMu[p.slot].Lock()
		h.snapshot.Slots[p.slot].ProbeReplies++
		h.snapshot.Slots[p.slot].LastRTT = rtt
		h.slotMu[p.slot].Unlock()
	}
	if p.arrival >= 0 && p.arrival <= ConnPoolSize {
		h.slotMu[p.arrival].Lock()
		h.snapshot.Slots[p.arrival].RepliesArrived++
		h.slotMu[p.arrival].Unlock()
	}
}

func (h *HeartbeatStats) finishProbe(id uint64, success bool) {
	if h == nil || id == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire(h.clock())
	p, ok := h.pending[id]
	if !ok {
		return
	}
	if !success {
		delete(h.pending, id)
		return
	}
	h.snapshot.Sent++
	if p.slot >= 0 && p.slot <= ConnPoolSize {
		h.slotMu[p.slot].Lock()
		h.snapshot.Slots[p.slot].ProbesSent++
		h.slotMu[p.slot].Unlock()
	}
	p.written = true
	if !p.reply.IsZero() {
		h.accept(id, p)
	} else {
		h.pending[id] = p
	}
}

// observeReply returns true only for protocol-owned echoes (including stale or
// duplicate ones). Application gateway echoes remain business packets for the TUN.
func (h *HeartbeatStats) observeReply(ip []byte, generation uint64, arrival int) bool {
	if h == nil || !isHeartbeatEchoReply(ip) {
		return false
	}
	off := echoOffset(ip)
	if off < 0 || len(ip) != off+8+probePayloadSize {
		return false
	}
	payload := ip[off+8:]
	if !bytes.Equal(payload[:8], []byte(probeMagic)) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Magic alone is not ownership: applications may echo the same layout.
	if !h.initialized || !bytes.Equal(payload[8:24], h.nonce[:]) {
		return false
	}
	now := h.clock()
	h.expire(now)
	id := binary.BigEndian.Uint64(payload[32:])
	p, ok := h.pending[id]
	destination := ip[16:20]
	if ip[0]>>4 == 6 {
		destination = ip[24:40]
	}
	if !ok || generation != h.snapshot.Generation || binary.BigEndian.Uint64(payload[24:32]) != h.snapshot.Generation || p.identity != [4]byte(ip[off+4:off+8]) || p.destination != string(destination) || !p.reply.IsZero() {
		h.snapshot.Rejected++
		return true
	}
	p.reply, p.arrival = now, arrival
	if p.written {
		h.accept(id, p)
	} else {
		h.pending[id] = p
	}
	return true
}

func (h *HeartbeatStats) slotActivity(slot int, generation uint64, read bool, n int) {
	if h == nil || slot < 0 || slot > ConnPoolSize {
		return
	}
	h.slotMu[slot].Lock()
	defer h.slotMu[slot].Unlock()
	s := &h.snapshot.Slots[slot]
	if generation != s.Generation {
		return
	}
	s.Generation = generation
	if read {
		s.LastRead = h.clock()
		s.ReadPackets++
		s.ReadBytes += uint64(n)
	} else {
		s.LastWrite = h.clock()
		s.WritePackets++
		s.WriteBytes += uint64(n)
	}
}

func (h *HeartbeatStats) slotTransition(slot int, generation uint64, connected bool, reason string) {
	if h == nil || slot < 0 || slot > ConnPoolSize {
		return
	}
	h.slotMu[slot].Lock()
	defer h.slotMu[slot].Unlock()
	s := &h.snapshot.Slots[slot]
	if generation != s.Generation {
		return
	}
	s.Generation, s.Connected = generation, connected
	if !connected && reason != "" {
		s.LastReconnect = h.clock()
		s.Reconnects++
		if len(reason) > 160 {
			reason = reason[:160]
		}
		s.LastReconnectReason = strings.Clone(reason)
	}
}
