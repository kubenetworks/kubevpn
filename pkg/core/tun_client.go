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
	// ConnPoolSize is the number of parallel TCP connections to the server.
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
	// slots holds the per-connection pool; built by runConnPool, read by routeOutbound.
	slots []*connSlot
	// gvisorInbound carries src == dst packets to the local loopback (self-to-self) gvisor stack.
	gvisorInbound chan *Packet
	// interClientInbound carries inter-client packets (type == packetTypeToGvisor, from the server)
	// to a single transport-level gvisor stack. It is NOT per-slot: the stack's lifetime is the
	// whole connection, so tearing down/reconnecting one pool slot never destroys an in-flight
	// inter-client transfer. The stack's output goes to the shared tunInbound (dispatched to a slot
	// by five-tuple hash), decoupling it from any one slot.
	interClientInbound chan *Packet
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
	// poolSize overrides the number of parallel connections; <=0 means use ConnPoolSize.
	// Production leaves it 0 (ConnPoolSize); only benchmarks/tests set it to compare sizes.
	poolSize int
}

func newClientTransport(dev *tunDevice, forward *Forwarder, stats *HeartbeatStats) *clientTransport {
	return &clientTransport{
		dev:                dev,
		forward:            forward,
		gvisorInbound:      make(chan *Packet, MaxSize),
		interClientInbound: make(chan *Packet, MaxSize),
		stats:              stats,
	}
}

func (t *clientTransport) label() string { return "[Client]" }

func (t *clientTransport) routines() []namedRoutine {
	return []namedRoutine{
		{"client-gvisor", func(ctx context.Context) {
			handleGvisorPacket(t.gvisorInbound, t.dev.tunOutbound, datagramHeaderLen).Run(ctx)
		}},
		{"client-gvisor-inter", func(ctx context.Context) {
			// Inter-client stack: shared across all pool slots (not bound to any slot's lifetime).
			// Its output goes to the shared tunInbound, which runConnPool dispatches to a slot by
			// five-tuple hash — so reconnecting a slot never tears down an active inter-client flow.
			handleGvisorPacket(t.interClientInbound, t.dev.tunInbound, datagramHeaderLen).Run(ctx)
		}},
		{"client-conn-pool", t.runConnPool},
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

// runConnPool creates N parallel connection slots and distributes packets by five-tuple hash. Each
// slot runs independently — if one connection breaks, only that slot reconnects. It waits for all
// slot goroutines to exit before returning, for deterministic shutdown.
func (t *clientTransport) runConnPool(ctx context.Context) {
	n := t.poolSize
	if n <= 0 {
		n = ConnPoolSize
	}
	t.slots = make([]*connSlot, n)
	var wg sync.WaitGroup
	for i := range t.slots {
		slot := &connSlot{
			id:                 i,
			inbound:            make(chan *Packet, MaxSize),
			tunOutbound:        t.dev.tunOutbound,
			forward:            t.forward,
			stats:              t.stats,
			registrations:      t.registrationPayloads,
			interClientInbound: t.interClientInbound,
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
	// Heartbeats (dst == nil) are broadcast to ALL slots to keep idle connections alive.
	for {
		select {
		case packet := <-t.dev.tunInbound:
			if packet == nil {
				return
			}
			if packet.dst != nil {
				// IP payload is data[tunReserve : datagramHeaderLen+length] (see packet.go layout).
				key := parseFiveTupleInline(packet.data[tunReserve : datagramHeaderLen+packet.length])
				trySendToSlot(t.slots[flowHash(key, packet.dst, n)].inbound, packet)
			} else {
				broadcastToSlots(t.slots, packet)
			}
		case <-ctx.Done():
			return
		}
	}
}

// registrationPayloads builds the proactive route-registration payloads — one ICMP echo to the
// gateway per TUN address family, each prefixed with the gvisor type byte (canonical layout, no
// datagram length header: UDPConnOverTCP.Write frames that on send). A slot writes these on every
// (re)connect so the server registers the route for that conn immediately. The echo also doubles
// as a liveness ping (its reply marks HeartbeatStats). Returns nil if the TUN IPs are unavailable.
func (t *clientTransport) registrationPayloads() [][]byte {
	tunIfi, err := netutil.GetTunDeviceByConn(t.dev.tun)
	if err != nil {
		return nil
	}
	srcIPv4, srcIPv6, _, _ := netutil.GetTunDeviceIP(tunIfi.Name)
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
		}
	}
	if srcIPv6 != nil {
		if icmp, e := netutil.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e == nil {
			appendPayload(icmp)
		}
	}
	return payloads
}

func (t *clientTransport) heartbeats(ctx context.Context) {
	tunIfi, err := netutil.GetTunDeviceByConn(t.dev.tun)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get tun device: %v", err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	sendHeartbeat := func(payload []byte) {
		buf := config.LPool.Get().([]byte)
		n := copy(buf[tunReserve:], payload)
		buf[datagramHeaderLen] = packetTypeToGvisor
		t.dev.tunInbound <- NewPacket(buf, n+typePrefixLen, nil, nil)
	}

	sendAll := func(reason string) {
		srcIPv4, srcIPv6, dockerSrcIPv4, _ := netutil.GetTunDeviceIP(tunIfi.Name)
		plog.G(ctx).Debugf("[Client] Sending heartbeat (%s)", reason)
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
