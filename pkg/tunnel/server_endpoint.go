package tunnel

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/google/gopacket/layers"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// readFromEndpointWriteToRoute reads packets from a per-client gvisor stack's endpoint and
// routes them to the correct client via Router. Each client has its own reader goroutine,
// so blocking on a slow client only affects that client's stack.
func (h *peerRegistry) readFromEndpointWriteToRoute(ctx context.Context, endpoint *channel.Endpoint) {
	stage := diagnosticStage(ctx, "route_dispatch", "server", -1, 0)
	defer stage.close()
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt == nil {
			continue
		}
		logStackPacket(ctx, "[gVISOR] ", sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)

		if tunReserve+pkt.Size() > config.LargeBufferSize {
			h.drops.report(ctx, dropOversized, nil)
			pkt.DecRef()
			continue
		}
		p := copyPacketToPool(pkt, packetTypeToTUN, datagramHeaderLen)
		_, dstIP, _, parseErr := netutil.ParseIPFast(p.ip())
		if parseErr != nil {
			h.drops.report(ctx, dropInvalid, nil)
			p.release()
			continue
		}
		p.dst = dstIP
		token := stage.begin()
		usedConn, err := h.writer.writeToRoutePacket(ctx, string(dstIP), p)
		stage.finish(token, 1, p.length, err == nil && ctx.Err() == nil)
		if err != nil {
			h.reportRouteDrop(ctx, err, dstIP)
		} else if plog.IsDebugEnabled(ctx) {
			plog.G(ctx).Debugf("[Gvisor-TCP] Stack output -> %s via %s", net.IP(dstIP), usedConn.RemoteAddr())
		}
		p.release()
	}
}

type serverConnRole uint8

const (
	serverConnRoleUnknown serverConnRole = iota
	serverConnRoleData
	serverConnRoleControl
)

// readFromTCPConnWriteToEndpoint owns connection framing, deadlines and role
// detection. Data packets are consumed by dispatchServerDataPacket; a control
// first datagram transfers the framed connection to the dedicated control loop.
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn) {
	inject := diagnosticStage(ctx, "gvisor_inject", "server", -1, 0)
	defer inject.close()
	routeStage := diagnosticStage(ctx, "route_dispatch", "server", -1, 0)
	defer routeStage.close()
	readStage := diagnosticStage(ctx, "socket_read", "server", -1, 0)
	defer readStage.close()
	tunEnqueue := diagnosticStage(ctx, "tun_out_enqueue", "server", -1, 0)
	defer tunEnqueue.close()
	tcpConn, _ := framing.NewConn(ctx, conn)
	defer tcpConn.Close()
	defer h.table.removeRoutesByConn(ctx, conn)
	role := serverConnRoleUnknown

	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: tcpConn.SetReadDeadline}
	reader := packetReader{conn: tcpConn}
	for ctx.Err() == nil {
		if err := dl.refresh(); err != nil {
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to set read deadline: %v", err)
			return
		}
		packet, err := reader.read()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Connection closed: %v", err)
			} else {
				plog.G(ctx).Errorf("[Gvisor-TCP] Failed to read from tcp conn: %v", err)
			}
			return
		}
		readStage.progress(1, packet.length)
		if packet.length == 0 {
			plog.G(ctx).Warnf("[Gvisor-TCP] Read empty packet from conn (length=%d)", packet.length)
			packet.release()
			continue
		}
		if role == serverConnRoleUnknown {
			if packet.kind() == packetKindControl {
				role = serverConnRoleControl
				packet.release()
				plog.G(ctx).Debugf("[Gvisor-TCP] Control conn detected: %s", conn.RemoteAddr())
				h.handleControlConn(ctx, tcpConn)
				return
			}
			role = serverConnRoleData
		}
		if !h.dispatchServerDataPacket(ctx, conn, packet, inject, routeStage, tunEnqueue) {
			return
		}
	}
}

// dispatchServerDataPacket consumes packet and preserves the server routing
// priority: direct client route, no-route cluster drop, gVisor, then TUN bridge.
// It returns false when the connection read loop must terminate.
func (h *gvisorTCPHandler) dispatchServerDataPacket(ctx context.Context, conn net.Conn, packet *packet, inject, routeStage, tunEnqueue *datapathStage) bool {
	read := packet.length
	ip := packet.ip()
	var protocol tcpip.NetworkProtocolNumber
	if netutil.IsIPv4(ip) {
		protocol = header.IPv4ProtocolNumber
	} else if netutil.IsIPv6(ip) {
		protocol = header.IPv6ProtocolNumber
	} else {
		h.drops.report(ctx, dropInvalid, nil)
		packet.release()
		return true
	}

	src, dst, ipProtocol, parseErr := netutil.ParseIPFast(ip)
	if parseErr != nil {
		h.drops.report(ctx, dropInvalid, nil)
		packet.release()
		return true
	}
	packet.src, packet.dst = src, dst
	inject.setPeer(src)
	routeStage.setPeer(src)
	if !h.table.addRoute(ctx, src, conn) {
		packet.release()
		return false
	}
	dstKey := string(dst)
	if h.table.hasRoute(dstKey) {
		packet = packet.detach()
		token := routeStage.begin()
		usedConn, writeErr := h.writer.writeToRoutePacket(ctx, dstKey, packet)
		routeStage.finish(token, 1, read, writeErr == nil && ctx.Err() == nil)
		if writeErr != nil {
			h.reportRouteDrop(ctx, writeErr, dst)
		} else if plog.IsDebugEnabled(ctx) {
			plog.G(ctx).Debugf("[Gvisor-TCP] Routed %s -> %s via %s", src, dst, usedConn.RemoteAddr())
		}
		packet.release()
		return true
	}
	if (config.CIDR.Contains(dst) || config.CIDR6.Contains(dst)) && ipProtocol != 1 && ipProtocol != 58 {
		h.drops.report(ctx, dropRouteMissing, dst)
		packet.release()
		return true
	}
	if packet.kind() == packetKindToGvisor {
		cs := h.getOrCreateClientStack(string(src))
		if cs == nil {
			packet.release()
			return false
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: 0,
			Payload:            buffer.MakeWithData(ip),
		})
		var logSrc, logDst net.IP
		if plog.IsDebugEnabled(ctx) {
			// src/dst alias packet storage; retain independent addresses after release.
			logSrc, logDst = append(net.IP(nil), src...), append(net.IP(nil), dst...)
		}
		packet.release()
		logStackPacket(ctx, "[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
		token := inject.begin()
		cs.endpoint.InjectInbound(protocol, pkt)
		inject.finish(token, 1, read, ctx.Err() == nil)
		pkt.DecRef()
		if logSrc != nil {
			plog.G(ctx).Debugf("[Gvisor-TCP] Injected to stack: %s -> %s, protocol=%s, length=%d", logSrc, logDst, layers.IPProtocol(ipProtocol).String(), read)
		}
		return true
	}
	packet = packet.detach()
	if !h.runtime.bridge.sendObserved(ctx, packet, tunEnqueue) {
		packet.release()
		return false
	}
	return true
}
