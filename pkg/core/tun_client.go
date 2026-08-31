package core

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

const (
	// ConnPoolSize is the number of parallel data TCP connections to the server.
	// Each connection handles a subset of traffic (partitioned by five-tuple hash —
	// proto, dst IP, and both ports — falling back to dst IP hash for fragments/ICMP).
	// Multiple connections reduce head-of-line blocking and improve throughput.
	ConnPoolSize = 4
)

// clientTransport is the spoke/endpoint half of the tun device. Outbound packets read from the
// TUN are distributed across a pool of dialed connections to the server (by five-tuple hash), or
// looped back through a local gvisor stack when src == dst. Inbound packets from the server are
// delivered to the device's tunOutbound by the per-connection readers.
type clientTransport struct {
	dev     *tunDevice
	forward *Forwarder
	// slots holds the data connection pool; built by runConnPool, read by routeOutbound.
	slots []*connSlot
	// controlSlot is a dedicated connection for heartbeat (control plane), independent of data
	// slots so that data-plane congestion cannot block liveness probes.
	controlSlot *connSlot
	// gvisorInbound carries src == dst packets to the local loopback (self-to-self) gvisor stack.
	gvisorInbound chan *Packet
	// interClient is the single transport-level gvisor stack that terminates inbound inter-client
	// traffic (type == packetTypeToGvisor, from the server). It is NOT per-slot: its lifetime is the
	// whole connection, so reconnecting one pool slot never destroys an in-flight inter-client
	// transfer. Each slot's reader injects directly into it (interClientStack.InjectIP) — no
	// intermediate bounded channel, so TCP's receive window (not a blind drop) throttles the peer.
	// Created by runConnPool before the slots, whose readers reference it. Its output goes to the
	// shared tunInbound (dispatched to a slot by five-tuple hash), decoupling it from any one slot.
	interClient *interClientStack
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
	// poolSize overrides the number of parallel connections; <=0 means use ConnPoolSize.
	// Production leaves it 0 (ConnPoolSize); only benchmarks/tests set it to compare sizes.
	poolSize int
}

func newClientTransport(dev *tunDevice, forward *Forwarder, stats *HeartbeatStats) *clientTransport {
	return &clientTransport{
		dev:           dev,
		forward:       forward,
		gvisorInbound: make(chan *Packet, MaxSize),
		stats:         stats,
	}
}

func (t *clientTransport) label() string { return "[Client]" }

func (t *clientTransport) routines() []namedRoutine {
	return []namedRoutine{
		{"client-gvisor", func(ctx context.Context) {
			handleGvisorPacket(t.gvisorInbound, t.dev.tunOutbound, datagramHeaderLen).Run(ctx)
		}},
		// The inter-client stack is created inside runConnPool (before its slots), because each
		// slot's reader injects into it directly; it is not a channel-fed routine of its own.
		{"client-conn-pool", t.runConnPool},
		{"client-control-slot", t.runControlSlot},
		{"client-heartbeat", t.heartbeats},
	}
}

// routeOutbound dispatches a packet read from the TUN: loop it back through the local gvisor
// stack when src == dst, otherwise enqueue it for the connection pool (distributed by five-tuple).
func (t *clientTransport) routeOutbound(ctx context.Context, buf []byte, n int, src, dst net.IP) {
	// buf is canonical (pumpTun reserved buf[0:tunReserve]): set the type prefix and the
	// IP already sits at buf[tunReserve:]. Both branches forward the same canonical buffer.
	buf[datagramHeaderLen] = packetTypeToGvisor
	logIPPacket(ctx, "[Client] OUTBOUND", buf[tunReserve:tunReserve+n])
	if src.Equal(dst) {
		t.gvisorInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	} else {
		// Enqueue to the shared tunInbound; runConnPool distributes to a slot by five-tuple hash.
		t.dev.tunInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	}
}

// runConnPool creates N parallel data connection slots and distributes packets by five-tuple hash.
// Data packets use blocking sends to provide TCP backpressure (prevents packet drops that trigger
// RTO stalls). Each slot runs independently — if one connection breaks, only that slot reconnects.
func (t *clientTransport) runConnPool(ctx context.Context) {
	n := t.poolSize
	if n <= 0 {
		n = ConnPoolSize
	}
	// Create the shared inter-client stack before the slots: their readers inject inbound
	// inter-client packets directly into it. Its output (replies to peers) goes to tunInbound,
	// which this function's loop below dispatches to a slot by five-tuple hash.
	t.interClient = newInterClientStack(ctx, t.dev.tunInbound, datagramHeaderLen)
	t.slots = make([]*connSlot, n)
	var wg sync.WaitGroup
	for i := range t.slots {
		slot := &connSlot{
			id:            i,
			inbound:       make(chan *Packet, MaxSize),
			tunOutbound:   t.dev.tunOutbound,
			forward:       t.forward,
			stats:         t.stats,
			registrations: t.registrationPayloads,
			interClient:   t.interClient,
		}
		t.slots[i] = slot
		wg.Add(1)
		go func(s *connSlot) {
			defer wg.Done()
			defer netutil.HandleCrash()
			s.run(ctx)
		}(slot)
	}
	defer wg.Wait()

	// Drain the shared tunInbound and distribute to slots by five-tuple hash.
	// Data packets block on the slot channel to propagate backpressure to the OS TCP stack,
	// preventing silent drops that cause catastrophic RTO stalls.
	for {
		select {
		case packet := <-t.dev.tunInbound:
			if packet == nil {
				return
			}
			if packet.dst != nil {
				key := parseFiveTupleInline(packet.data[tunReserve : datagramHeaderLen+packet.length])
				select {
				case t.slots[flowHash(key, packet.dst, n)].inbound <- packet:
				case <-ctx.Done():
					packet.release()
					return
				}
			} else {
				// Fallback broadcast for non-heartbeat control packets (currently unused;
				// heartbeats bypass tunInbound via controlSlot).
				broadcastToSlots(t.slots, packet)
			}
		case <-ctx.Done():
			return
		}
	}
}

// runControlSlot manages the dedicated control-plane connection. It carries only heartbeat
// ICMP echo packets, isolated from data-plane congestion so the liveness watchdog is never
// starved by a full data slot. The server recognizes this conn by the packetTypeControl prefix
// on its first datagram and does NOT register it in RouteHub.
func (t *clientTransport) runControlSlot(ctx context.Context) {
	t.controlSlot = &connSlot{
		id:          ConnPoolSize,
		inbound:     make(chan *Packet, MaxSize),
		tunOutbound: t.dev.tunOutbound,
		forward:     t.forward,
		stats:       t.stats,
		isControl:   true,
	}
	t.controlSlot.run(ctx)
}

// registrationPayloads builds the proactive route-registration payloads — one ICMP echo to the
// gateway per TUN address family, each prefixed with the gvisor type byte (canonical layout, no
// datagram length header: UDPConnOverTCP.Write frames that on send). A slot writes these on every
// (re)connect so the server registers the route for that conn immediately. The echo also doubles
// as a liveness ping (its reply marks HeartbeatStats). Returns nil if the TUN IPs are unavailable.
func (t *clientTransport) registrationPayloads(ctx context.Context) [][]byte {
	// Look the addresses up by interface NAME (resolved once at device creation), never by
	// re-scanning the interface table: this runs on every slot (re)connect, and a failed scan
	// here means the server never learns our route — every heartbeat echo reply it generates is
	// then dropped for want of a route, so the liveness watchdog force-reconnects forever.
	srcIPv4, srcIPv6, _ := t.dev.addrs()
	var payloads [][]byte
	appendPayload := func(icmp []byte) {
		payload := make([]byte, typePrefixLen+len(icmp))
		payload[0] = packetTypeToGvisor
		copy(payload[typePrefixLen:], icmp)
		payloads = append(payloads, payload)
	}
	if srcIPv4 != nil {
		if icmp, e := netutil.GenICMPPacket(srcIPv4, config.RouterIP); e == nil {
			appendPayload(icmp)
		} else {
			dataPlaneWarn.Warnf(ctx, "reg-gen-v4", "[Client] Failed to build IPv4 route announcement: %v", e)
		}
	}
	if srcIPv6 != nil {
		if icmp, e := netutil.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e == nil {
			appendPayload(icmp)
		} else {
			dataPlaneWarn.Warnf(ctx, "reg-gen-v6", "[Client] Failed to build IPv6 route announcement: %v", e)
		}
	}
	if len(payloads) == 0 {
		// Announcing nothing is not a benign no-op: the server keeps no route for us, so every
		// heartbeat echo reply it generates is dropped and the tunnel is dead for an idle client
		// even though all its connections look healthy. Never let this be silent again.
		dataPlaneWarn.Warnf(ctx, "reg-empty",
			"[Client] Cannot announce our route: no TUN address available on %q. Inbound traffic and "+
				"heartbeat replies will be dropped by the server until this resolves", t.dev.tunName)
	}
	return payloads
}

// heartbeats sends periodic ICMP echo packets via the dedicated controlSlot, bypassing the data
// tunInbound path entirely. This ensures liveness probes flow even when data slots are congested.
func (t *clientTransport) heartbeats(ctx context.Context) {
	// No fail-fast on an unresolvable device here: a transient lookup failure must not disable
	// liveness for the rest of the session (that failure mode is exactly what black-holed the
	// tunnel). sendAll re-reads the addresses every tick and warns (throttled) while they are
	// unavailable, so the heartbeat recovers on its own once they come back.
	ticker := time.NewTicker(config.HeartbeatInterval)
	defer ticker.Stop()

	// Every way a heartbeat can fail to leave the host is reported (throttled): a heartbeat that is
	// never sent is indistinguishable at the watchdog from a black-holed tunnel, and it will
	// force-reconnect the port-forward every livenessStartupDeadline forever on the strength of it.
	sendHeartbeat := func(payload []byte) {
		if t.controlSlot == nil {
			dataPlaneWarn.Warnf(ctx, "hb-no-control-slot", "[Client] Heartbeat skipped: control slot not up yet")
			return
		}
		buf := config.LPool.Get().([]byte)
		n := copy(buf[tunReserve:], payload)
		buf[datagramHeaderLen] = packetTypeControl
		if !trySendToSlot(t.controlSlot.inbound, NewPacket(buf, n+typePrefixLen, nil, nil)) {
			dataPlaneWarn.Warnf(ctx, "hb-slot-full", "[Client] Heartbeat dropped: control slot queue full")
		}
	}

	sendAll := func(reason string) {
		srcIPv4, srcIPv6, dockerSrcIPv4 := t.dev.addrs()
		plog.G(ctx).Debugf("[Client] Sending heartbeat (%s)", reason)
		if srcIPv4 == nil && srcIPv6 == nil {
			dataPlaneWarn.Warnf(ctx, "hb-no-addr",
				"[Client] Heartbeat not sent: no TUN address available on %q; the data plane will be "+
					"reported unhealthy until this resolves", t.dev.tunName)
		}
		if srcIPv4 != nil {
			if icmp, e := netutil.GenICMPPacket(srcIPv4, config.RouterIP); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv4 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if srcIPv6 != nil {
			if icmp, e := netutil.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv6 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if dockerSrcIPv4 != nil {
			_, _ = netutil.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}

	sendAll("initial")
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			sendAll("periodic")
		case <-ctx.Done():
			return
		}
	}
}
