package tunnel

import (
	"context"
	"encoding/binary"
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

		buf := config.LPool.Get().([]byte)
		if tunReserve+pkt.Size() > config.LargeBufferSize {
			h.drops.report(ctx, dropOversized, nil)
			config.LPool.Put(buf)
			pkt.DecRef()
			continue
		}
		dst := buf[tunReserve:]
		ipLen := 0
		for _, s := range pkt.AsSlices() {
			ipLen += copy(dst[ipLen:], s)
		}
		pkt.DecRef()

		_, dstIP, _, parseErr := netutil.ParseIPFast(buf[tunReserve : tunReserve+ipLen])
		if parseErr != nil {
			h.drops.report(ctx, dropInvalid, nil)
			config.LPool.Put(buf)
			continue
		}

		payloadLen := ipLen + typePrefixLen
		binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(payloadLen))
		buf[datagramHeaderLen] = packetTypeToTUN

		p := newPacket(buf, payloadLen, nil, dstIP)
		token := stage.begin()
		usedConn, err := h.hub.WriteToRoutePacket(ctx, string(dstIP), p)
		stage.finish(token, 1, payloadLen, err == nil && ctx.Err() == nil)
		if err != nil {
			h.reportRouteDrop(ctx, err, dstIP)
		} else if plog.IsDebugEnabled(ctx) {
			plog.G(ctx).Debugf("[Gvisor-TCP] Stack output -> %s via %s", net.IP(dstIP), usedConn.RemoteAddr())
		}
		p.release()
	}
}

// readFromTCPConnWriteToEndpoint reads framed packets from a tunnel connection, routes
// inter-client traffic directly via Router, and injects cluster-bound traffic into the
// appropriate per-client gvisor stack. If the first datagram is a control frame
// (packetTypeControl), the conn is handed off to handleControlConn instead.
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn) {
	inject := diagnosticStage(ctx, "gvisor_inject", "server", -1, 0)
	defer inject.close()
	dispatch := diagnosticStage(ctx, "route_dispatch", "server", -1, 0)
	defer dispatch.close()
	readStage := diagnosticStage(ctx, "socket_read", "server", -1, 0)
	defer readStage.close()
	tunEnqueue := diagnosticStage(ctx, "tun_out_enqueue", "server", -1, 0)
	defer tunEnqueue.close()
	tcpConn, _ := framing.NewConn(ctx, conn)
	defer tcpConn.Close()
	defer h.hub.RemoveRoutesByConn(ctx, conn)
	var firstPacket = true

	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: tcpConn.SetReadDeadline}

	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		if err := dl.refresh(); err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to set read deadline: %v", err)
			return
		}
		read, err := tcpConn.Read(buf[datagramHeaderLen:])
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Connection closed: %v", err)
			} else {
				plog.G(ctx).Errorf("[Gvisor-TCP] Failed to read from tcp conn: %v", err)
			}
			config.LPool.Put(buf[:])
			return
		}
		readStage.progress(1, read)
		if read == 0 {
			plog.G(ctx).Warnf("[Gvisor-TCP] Read empty packet from conn (length=%d)", read)
			config.LPool.Put(buf[:])
			continue
		}
		// First datagram declares conn type: control conns are handled separately.
		if firstPacket {
			firstPacket = false
			if buf[datagramHeaderLen] == packetTypeControl {
				config.LPool.Put(buf[:])
				plog.G(ctx).Debugf("[Gvisor-TCP] Control conn detected: %s", conn.RemoteAddr())
				h.handleControlConn(ctx, tcpConn)
				return
			}
		}

		ip := buf[tunReserve : datagramHeaderLen+read]
		var protocol tcpip.NetworkProtocolNumber
		if netutil.IsIPv4(ip) {
			protocol = header.IPv4ProtocolNumber
		} else if netutil.IsIPv6(ip) {
			protocol = header.IPv6ProtocolNumber
		} else {
			h.drops.report(ctx, dropInvalid, nil)
			config.LPool.Put(buf[:])
			continue
		}

		src, dst, ipProtocol, parseErr := netutil.ParseIPFast(ip)
		if parseErr != nil {
			h.drops.report(ctx, dropInvalid, nil)
			config.LPool.Put(buf[:])
			continue
		}

		inject.setPeer(src)
		dispatch.setPeer(src)
		if !h.hub.AddRoute(ctx, src, conn) {
			config.LPool.Put(buf[:])
			return
		}
		dstKey := string(dst)
		if h.hub.HasRoute(dstKey) {
			pkt := newPacket(buf, read, src, dst)
			binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(read))
			token := dispatch.begin()
			usedConn, writeErr := h.hub.WriteToRoutePacket(ctx, dstKey, pkt)
			dispatch.finish(token, 1, read, writeErr == nil && ctx.Err() == nil)
			if writeErr != nil {
				h.reportRouteDrop(ctx, writeErr, dst)
			} else if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Routed %s -> %s via %s", src, dst, usedConn.RemoteAddr())
			}
			pkt.release()
		} else if (config.CIDR.Contains(dst) || config.CIDR6.Contains(dst)) &&
			ipProtocol != 1 && ipProtocol != 58 {
			h.drops.report(ctx, dropRouteMissing, dst)
			config.LPool.Put(buf[:])
			continue
		} else if buf[datagramHeaderLen] == packetTypeToGvisor {
			cs := h.getOrCreateClientStack(string(src))
			if cs == nil {
				config.LPool.Put(buf[:])
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				ReserveHeaderBytes: 0,
				Payload:            buffer.MakeWithData(ip),
			})
			var logSrc, logDst net.IP
			if plog.IsDebugEnabled(ctx) {
				// src/dst alias buf; retain independent addresses for logging after the pool return.
				logSrc, logDst = append(net.IP(nil), src...), append(net.IP(nil), dst...)
			}
			config.LPool.Put(buf[:])
			logStackPacket(ctx, "[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
			token := inject.begin()
			cs.endpoint.InjectInbound(protocol, pkt)
			inject.finish(token, 1, read, ctx.Err() == nil)
			pkt.DecRef()
			if logSrc != nil {
				plog.G(ctx).Debugf("[Gvisor-TCP] Injected to stack: %s -> %s, protocol=%s, length=%d", logSrc, logDst, layers.IPProtocol(ipProtocol).String(), read)
			}
		} else {
			pkt := newPacket(buf[:], read, src, dst)
			if !h.hub.bridge.sendObserved(ctx, pkt, tunEnqueue) {
				pkt.release()
				return
			}
		}
	}
}
