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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (h *gvisorTCPHandler) readFromEndpointWriteToTCPConn(ctx context.Context, conn net.Conn, endpoint *channel.Endpoint) {
	for ctx.Err() == nil {
		pkt := endpoint.ReadContext(ctx)
		if pkt != nil {
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionSend, pkt.NetworkProtocolNumber, pkt)
			buf := config.LPool.Get().([]byte)
			// Single copy out of gvisor's section views (AsSlices aliases them) into the
			// pooled buffer at the canonical IP offset; ToView().AsSlice() would flatten
			// into a throwaway buffer first (a second copy).
			dst := buf[tunReserve:]
			ipLen := 0
			for _, s := range pkt.AsSlices() {
				ipLen += copy(dst[ipLen:], s)
			}
			pkt.DecRef()
			payloadLen := ipLen + typePrefixLen
			binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(payloadLen))
			buf[datagramHeaderLen] = packetTypeToTUN
			_, err := conn.Write(buf[:payloadLen+datagramHeaderLen])
			config.LPool.Put(buf)
			if err != nil {
				plog.G(ctx).Errorf("[Gvisor-TCP] Failed to write to conn: %v", err)
				return
			}
		}
	}
}

// tun --> dispatcher
func (h *gvisorTCPHandler) readFromTCPConnWriteToEndpoint(ctx context.Context, conn net.Conn, endpoint *channel.Endpoint) {
	tcpConn, _ := NewUDPConnOverTCP(ctx, conn)
	defer tcpConn.Close()
	defer h.hub.RemoveRoutesByConn(ctx, conn)

	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Read into buf[datagramHeaderLen:] so the stripped payload lands in canonical position:
		// buf[datagramHeaderLen] = type prefix, buf[tunReserve:] = IP. read = type+IP length.
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
		ip := buf[tunReserve : datagramHeaderLen+read]
		// Determine network protocol from IP version field (IFF_NO_PI mode)
		var protocol tcpip.NetworkProtocolNumber
		if util.IsIPv4(ip) {
			protocol = header.IPv4ProtocolNumber
		} else if util.IsIPv6(ip) {
			protocol = header.IPv6ProtocolNumber
		} else {
			plog.G(ctx).Errorf("[Gvisor-TCP] Unknown packet, dropping")
			config.LPool.Put(buf[:])
			continue
		}

		src, dst, ipProtocol, parseErr := util.ParseIPFast(ip)
		if parseErr != nil {
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to parse IP header: %v", parseErr)
			config.LPool.Put(buf[:])
			continue
		}

		h.hub.AddRoute(ctx, src, conn)
		dstKey := string(dst)
		if h.hub.HasRoute(dstKey) {
			// Zero-copy: buf is canonical (IP at buf[tunReserve:], 2 bytes of headroom at
			// buf[:datagramHeaderLen]). Stamp the length in place and hand the packet to the
			// route by reference; the chosen conn takes a reference and we drop ours below.
			pkt := NewPacket(buf, read, src, dst)
			binary.BigEndian.PutUint16(buf[:datagramHeaderLen], uint16(read))
			usedConn, writeErr := h.hub.WriteToRoutePacket(dstKey, pkt)
			pkt.release()
			if writeErr != nil {
				plog.G(ctx).Warnf("[Gvisor-TCP] All routes dead for %s: %v", dst, writeErr)
			} else if config.Debug {
				plog.G(ctx).Debugf("[Gvisor-TCP] Routed %s -> %s via %s", src, dst, usedConn.RemoteAddr())
			}
		} else if buf[datagramHeaderLen] == packetTypeToGvisor {
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				ReserveHeaderBytes: 0,
				Payload:            buffer.MakeWithData(ip),
			})
			config.LPool.Put(buf[:])
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
			endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
			if config.Debug {
				plog.G(ctx).Debugf("[Gvisor-TCP] Injected to stack: %s -> %s, protocol=%s, length=%d", src, dst, layers.IPProtocol(ipProtocol).String(), read)
			}
		} else {
			pkt := NewPacket(buf[:], read, src, dst)
			// Honor cancellation so this goroutine never blocks forever on a full
			// channel after the consumer (routeTCPToTun) has exited at shutdown.
			select {
			case h.hub.TCPPacketChan <- pkt:
			case <-ctx.Done():
				pkt.release()
				return
			}
		}
	}
}
