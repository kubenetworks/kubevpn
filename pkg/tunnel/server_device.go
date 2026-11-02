package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"net"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// serverTransport is the traffic-manager (hub) half of the tun device. Packets read from the
// server's TUN are routed to the owning client connection via Router; packets that the gvisor
// stacks could not route locally arrive on Router.bridge.queue and are written back to the TUN.
type serverTransport struct {
	dev *tunDevice
	hub *Router
}

func newServerTransport(dev *tunDevice, hub *Router) *serverTransport {
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
	packet := newPacket(buf[:], n+typePrefixLen, src, dst)
	if !diagnosticEnqueue(ctx, t.dev.tunInbound, packet, t.dev.enqueueStage) {
		packet.release()
	}
}

// routeTun drains tunInbound and writes each packet to the connection registered for its
// destination IP in Router (with dead-conn fallback inside WriteToRoute).
func (t *serverTransport) routeTun(ctx context.Context) {
	stage := diagnosticStage(ctx, "route_dispatch", "tun", -1, 0)
	defer stage.close()
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
			token := stage.begin()
			conn, err := t.hub.WriteToRoutePacket(ctx, dstKey, packet)
			stage.finish(token, 1, packet.length, err == nil && ctx.Err() == nil)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					reason := dropRoutesFailed
					if errors.Is(err, errNoRoute) {
						reason = dropRouteMissing
					}
					t.dev.drops.report(ctx, reason, packet.dst)
				}
			} else if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[TUN] Routed %s -> %s via %s", packet.src, packet.dst, conn.RemoteAddr())
			}
			packet.release()
		case <-ctx.Done():
			return
		}
	}
}

// routeTCPToTun bridges packets that the gvisor stacks could not route locally (delivered on
// Router.bridge.queue) back to the server's TUN via tunOutbound.
func (t *serverTransport) routeTCPToTun(ctx context.Context) {
	stage := diagnosticStage(ctx, "tun_out_enqueue", "tun", -1, 0)
	defer stage.close()
	defer t.hub.bridge.stop()
	for {
		select {
		case packet := <-t.hub.bridge.queue:
			if packet == nil {
				return
			}
			if !diagnosticEnqueue(ctx, t.dev.tunOutbound, packet, stage) {
				packet.release()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
