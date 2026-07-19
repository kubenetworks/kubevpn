package core

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
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// readFromEndpointWriteToRoute reads packets from a per-client gvisor stack's endpoint and
// routes them to the correct client via RouteHub. Each client has its own reader goroutine,
// so blocking on a slow client only affects that client's stack.
func (h *gvisorTCPHandler) readFromEndpointWriteToRoute(ctx context.Context, endpoint *channel.Endpoint) {
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt == nil {
			continue
		}
		sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)

		buf := config.LPool.Get().([]byte)
		if tunReserve+pkt.Size() > config.LargeBufferSize {
			plog.G(ctx).Warnf("[Gvisor-TCP] dropping oversized packet (%d bytes) that cannot be framed", pkt.Size())
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
			plog.G(ctx).Warnf("[Gvisor-TCP] Failed to parse stack output IP header: %v", parseErr)
			config.LPool.Put(buf)
			continue
		}

		payloadLen := ipLen + typePrefixLen
		binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(payloadLen))
		buf[datagramHeaderLen] = packetTypeToTUN

		p := NewPacket(buf, payloadLen, nil, dstIP)
		usedConn, err := h.hub.WriteToRoutePacket(string(dstIP), p)
		p.release()
		if err != nil {
			if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[Gvisor-TCP] No route for stack output -> %s, dropping", net.IP(dstIP))
			}
		} else if plog.IsDebugEnabled(ctx) {
			plog.G(ctx).Debugf("[Gvisor-TCP] Stack output -> %s via %s", net.IP(dstIP), usedConn.RemoteAddr())
		}
	}
}

// readFromTCPConnWriteToEndpoint reads framed packets from a tunnel connection, routes
// inter-client traffic directly via RouteHub, and injects cluster-bound traffic into the
// appropriate per-client gvisor stack. If the first datagram is a control frame
// (packetTypeControl), the conn is handed off to handleControlConn instead.
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn) {
	tcpConn, _ := NewUDPConnOverTCP(ctx, conn)
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
			plog.G(ctx).Errorf("[Gvisor-TCP] Unknown packet, dropping")
			config.LPool.Put(buf[:])
			continue
		}

		src, dst, ipProtocol, parseErr := netutil.ParseIPFast(ip)
		if parseErr != nil {
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to parse IP header: %v", parseErr)
			config.LPool.Put(buf[:])
			continue
		}

		h.hub.AddRoute(ctx, src, conn)
		dstKey := string(dst)
		if h.hub.HasRoute(dstKey) {
			pkt := NewPacket(buf, read, src, dst)
			binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(read))
			usedConn, writeErr := h.hub.WriteToRoutePacket(dstKey, pkt)
			pkt.release()
			if writeErr != nil {
				plog.G(ctx).Warnf("[Gvisor-TCP] All routes dead for %s: %v", dst, writeErr)
			} else if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Routed %s -> %s via %s", src, dst, usedConn.RemoteAddr())
			}
		} else if (config.CIDR.Contains(dst) || config.CIDR6.Contains(dst)) &&
			ipProtocol != 1 && ipProtocol != 58 {
			if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Peer route missing for %s, dropping TCP/UDP from %s", net.IP(dst), net.IP(src))
			}
			config.LPool.Put(buf[:])
			continue
		} else if buf[datagramHeaderLen] == packetTypeToGvisor {
			cs := h.getOrCreateClientStack(string(src))
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				ReserveHeaderBytes: 0,
				Payload:            buffer.MakeWithData(ip),
			})
			config.LPool.Put(buf[:])
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
			cs.endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
			if plog.IsDebugEnabled(ctx) {
				plog.G(ctx).Debugf("[Gvisor-TCP] Injected to stack: %s -> %s, protocol=%s, length=%d", src, dst, layers.IPProtocol(ipProtocol).String(), read)
			}
		} else {
			pkt := NewPacket(buf[:], read, src, dst)
			select {
			case h.hub.TCPPacketChan <- pkt:
			case <-ctx.Done():
				pkt.release()
				return
			}
		}
	}
}
