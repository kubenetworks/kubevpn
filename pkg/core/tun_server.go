package core

import (
	"context"
	"encoding/binary"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// slowPathWarnThreshold logs a perf warning when a packet hot-path operation blocks longer than this.
const slowPathWarnThreshold = 20 * time.Millisecond

// serverTransport is the traffic-manager (hub) half of the tun device. Packets read from the
// server's TUN are routed to the owning client connection via RouteHub; packets that the gvisor
// stacks could not route locally arrive on RouteHub.TCPPacketChan and are written back to the TUN.
type serverTransport struct {
	dev *tunDevice
	hub *RouteHub
}

func newServerTransport(dev *tunDevice, hub *RouteHub) *serverTransport {
	return &serverTransport{dev: dev, hub: hub}
}

func (t *serverTransport) label() string { return "[TUN]" }

func (t *serverTransport) routines() []namedRoutine {
	return []namedRoutine{
		{"server-route", t.routeTun},
		{"server-tcp-to-tun", t.routeTCPToTun},
	}
}

// routeOutbound hands a packet read from the TUN to the route goroutine. IP data sits at buf[3:]
// (pumpTun reserved buf[0:3] for the datagram length + type prefix), so routeTun can frame in place.
func (t *serverTransport) routeOutbound(ctx context.Context, buf []byte, n int, src, dst net.IP) {
	// Canonical layout: set the type prefix in place; IP already sits at buf[tunReserve:].
	// length is type+IP so routeTun can frame without further arithmetic.
	logIPPacket(ctx, "[TUN]", buf[tunReserve:tunReserve+n])
	buf[datagramHeaderLen] = packetTypeToGvisor
	sendStart := time.Now()
	t.dev.tunInbound <- NewPacket(buf[:], n+typePrefixLen, src, dst)
	if elapsed := time.Since(sendStart); elapsed > slowPathWarnThreshold {
		plog.G(ctx).Warnf("[Perf] Slow tunInbound send: %s -> %s blocked %v (channel backpressure)", src, dst, elapsed)
	}
}

// routeTun drains tunInbound and writes each packet to the connection registered for its
// destination IP in RouteHub (with dead-conn fallback inside WriteToRoute).
func (t *serverTransport) routeTun(ctx context.Context) {
	defer drainPacketChan(t.dev.tunInbound)
	for {
		select {
		case packet := <-t.dev.tunInbound:
			if packet == nil {
				return
			}
			dstKey := string(packet.dst)
			// Canonical layout: type prefix already set by routeOutbound, length is type+IP.
			// Frame the datagram length in place: buf[0:datagramHeaderLen]. Hand the packet to
			// the route by reference (zero-copy); the chosen conn takes a reference and we drop
			// ours below.
			binary.BigEndian.PutUint16(packet.data[:datagramHeaderLen], uint16(packet.length))
			writeStart := time.Now()
			conn, err := t.hub.WriteToRoutePacket(dstKey, packet)
			if elapsed := time.Since(writeStart); elapsed > slowPathWarnThreshold {
				plog.G(ctx).Warnf("[Perf] Slow WriteToRoute: %s -> %s took %v", packet.src, packet.dst, elapsed)
			}
			packet.release()
			if err != nil {
				plog.G(ctx).Warnf("[TUN] No route for %s -> %s, dropping", packet.src, packet.dst)
			} else if config.Debug {
				plog.G(ctx).Debugf("[TUN] Routed %s -> %s via %s", packet.src, packet.dst, conn.RemoteAddr())
			}
		case <-ctx.Done():
			return
		}
	}
}

// routeTCPToTun bridges packets that the gvisor stacks could not route locally (delivered on
// RouteHub.TCPPacketChan) back to the server's TUN via tunOutbound.
func (t *serverTransport) routeTCPToTun(ctx context.Context) {
	defer drainPacketChan(t.hub.TCPPacketChan)
	for {
		select {
		case packet := <-t.hub.TCPPacketChan:
			if packet == nil {
				return
			}
			t.dev.tunOutbound <- packet
		case <-ctx.Done():
			return
		}
	}
}
