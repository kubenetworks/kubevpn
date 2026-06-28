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
			view := pkt.ToView()
			data := view.AsSlice()
			buf := config.LPool.Get().([]byte)
			payloadLen := len(data) + 1
			binary.BigEndian.PutUint16(buf[:2], uint16(payloadLen))
			buf[2] = 0
			copy(buf[3:], data)
			view.Release()
			pkt.DecRef()
			_, err := conn.Write(buf[:payloadLen+2])
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
		read, err := tcpConn.Read(buf[:])
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
		// Determine network protocol from IP version field (IFF_NO_PI mode)
		var protocol tcpip.NetworkProtocolNumber
		if util.IsIPv4(buf[1:read]) {
			protocol = header.IPv4ProtocolNumber
		} else if util.IsIPv6(buf[1:read]) {
			protocol = header.IPv6ProtocolNumber
		} else {
			plog.G(ctx).Errorf("[Gvisor-TCP] Unknown packet, dropping")
			config.LPool.Put(buf[:])
			continue
		}

		src, dst, ipProtocol, parseErr := util.ParseIPFast(buf[1:read])
		if parseErr != nil {
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to parse IP header: %v", parseErr)
			config.LPool.Put(buf[:])
			continue
		}

		h.hub.AddRoute(ctx, src, conn)
		dstKey := string(dst)
		if h.hub.HasRoute(dstKey) {
			dgram := newDatagramPacket(buf, read)
			// Write with fallback: tries all conns for dst, removes dead ones
			usedConn, writeErr := h.hub.WriteFuncToRoute(dstKey, func(c net.Conn) error {
				return dgram.Write(c)
			})
			config.LPool.Put(buf[:])
			if writeErr != nil {
				plog.G(ctx).Warnf("[Gvisor-TCP] All routes dead for %s: %v", dst, writeErr)
			} else if config.Debug {
				plog.G(ctx).Debugf("[Gvisor-TCP] Routed %s -> %s via %s", src, dst, usedConn.RemoteAddr())
			}
		} else if buf[0] == 1 {
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
				ReserveHeaderBytes: 0,
				Payload:            buffer.MakeWithData(buf[1:read]),
			})
			config.LPool.Put(buf[:])
			sniffer.LogPacket("[gVISOR] ", sniffer.DirectionRecv, protocol, pkt)
			endpoint.InjectInbound(protocol, pkt)
			pkt.DecRef()
			if config.Debug {
				plog.G(ctx).Debugf("[Gvisor-TCP] Injected to stack: %s -> %s, protocol=%s, length=%d", src, dst, layers.IPProtocol(ipProtocol).String(), read)
			}
		} else {
			pkt := &Packet{
				data:   buf[:],
				length: read,
				src:    src,
				dst:    dst,
			}
			// Honor cancellation so this goroutine never blocks forever on a full
			// channel after the consumer (routeTCPToTun) has exited at shutdown.
			select {
			case h.hub.TCPPacketChan <- pkt:
			case <-ctx.Done():
				config.LPool.Put(buf[:])
				return
			}
		}
	}
}
