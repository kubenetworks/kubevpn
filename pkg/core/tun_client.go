package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/google/gopacket/layers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	// ConnPoolSize is the number of parallel TCP connections to the server.
	// Each connection handles a subset of traffic (partitioned by dst IP hash).
	// Multiple connections reduce head-of-line blocking and improve throughput.
	ConnPoolSize = 4
)

func (h *tunHandler) HandleClient(ctx context.Context, tun net.Conn, forwarder *Forwarder) {
	device := &ClientDevice{
		tunDevice: tunDevice{
			tun:         tun,
			tunInbound:  make(chan *Packet, MaxSize),
			tunOutbound: make(chan *Packet, MaxSize),
			errChan:     h.errChan,
		},
		reconnected: make(chan struct{}, 1),
	}

	defer device.Close()
	go device.runConnPool(ctx, forwarder)
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go device.heartbeats(ctx)
	select {
	case <-device.errChan:
	case <-ctx.Done():
	}
}

// ClientDevice is the client-side tun device handler.
type ClientDevice struct {
	tunDevice
	// reconnected signals the heartbeat goroutine to send an immediate heartbeat
	// after a new connection is established, so the server can register the route
	// without waiting for the next ticker cycle.
	reconnected chan struct{}

	// slots holds per-connection inbound channels for hash-based packet distribution.
	// Set by runConnPool, read by readFromTun.
	slots []chan *Packet
}

// runConnPool creates N parallel connections and distributes packets by dst IP hash.
// Each slot runs independently — if one connection breaks, only that slot reconnects.
func (d *ClientDevice) runConnPool(ctx context.Context, forward *Forwarder) {
	n := ConnPoolSize
	d.slots = make([]chan *Packet, n)
	for i := range d.slots {
		d.slots[i] = make(chan *Packet, MaxSize)
	}

	// Start N independent connection slots
	for i := 0; i < n; i++ {
		go d.runSlot(ctx, forward, i)
	}

	// Drain the shared tunInbound and distribute to slots by dst IP hash.
	// Heartbeats (dst == nil) are broadcast to ALL slots to keep idle connections alive.
	for {
		select {
		case packet := <-d.tunInbound:
			if packet == nil {
				return
			}
			if packet.dst != nil {
				select {
				case d.slots[ipHash(packet.dst, n)] <- packet:
				default:
					config.LPool.Put(packet.data[:])
					// slot channel full — drop packet to prevent cross-slot stall
				}
			} else {
				select {
				case d.slots[0] <- packet:
				default:
					config.LPool.Put(packet.data[:])
				}
				for i := 1; i < n; i++ {
					clone := config.LPool.Get().([]byte)
					copy(clone, packet.data[:packet.length+2])
					select {
					case d.slots[i] <- &Packet{data: clone, length: packet.length}:
					default:
						config.LPool.Put(clone[:])
						// slot channel full — drop heartbeat clone to prevent cross-slot stall
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// runSlot manages a single connection slot with reconnect logic.
func (d *ClientDevice) runSlot(ctx context.Context, forward *Forwarder, slotID int) {
	firstConnect := true
	for ctx.Err() == nil {
		func() {
			subCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			conn, err := forward.DialContext(subCtx)
			if err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to dial %s: %v", slotID, forward.Addr, err)
				time.Sleep(config.SlotReconnectBackoff)
				return
			}
			defer conn.Close()
			plog.G(ctx).Infof("[Client-%d] Connected to %s", slotID, conn.RemoteAddr())

			// Signal heartbeat on reconnect (not first connect — initial heartbeat covers that)
			if slotID == 0 && !firstConnect {
				select {
				case d.reconnected <- struct{}{}:
				default:
				}
			}
			firstConnect = false

			udpConn := conn.(*UDPConnOverTCP)
			errChan := make(chan error, 2)
			go readFromConn(subCtx, udpConn, d.slots[slotID], d.tunOutbound, errChan, slotID)
			go writeToConn(subCtx, udpConn.Conn, d.slots[slotID], errChan, slotID)

			select {
			case err = <-errChan:
				plog.G(ctx).Warnf("[Client-%d] Disconnected from %s: %v", slotID, conn.RemoteAddr(), err)
			case <-ctx.Done():
				return
			}
		}()
	}
}

// ipHash returns a consistent slot index for an IP address.
// Uses FNV-1a-like hash for fast, well-distributed mapping.
func ipHash(ip net.IP, slots int) int {
	var h uint32 = 2166136261
	for _, b := range ip {
		h ^= uint32(b)
		h *= 16777619
	}
	return int(h % uint32(slots))
}

func readFromConn(ctx context.Context, conn net.Conn, slotInbound chan *Packet, tunOutbound chan *Packet, errChan chan error, slotID int) {
	defer util.HandleCrash()
	gvisorInbound := make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, slotInbound, 2).Run(ctx)
	readTimeout := config.KeepAliveTime * 3
	nextDeadline := time.Now().Add(readTimeout)
	if err := conn.SetReadDeadline(nextDeadline); err != nil {
		plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", slotID, err)
		util.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
		return
	}
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Refresh deadline only when >half the timeout has elapsed, avoiding per-packet syscall
		if time.Until(nextDeadline) < readTimeout/2 {
			nextDeadline = time.Now().Add(readTimeout)
			if err := conn.SetReadDeadline(nextDeadline); err != nil {
				config.LPool.Put(buf[:])
				plog.G(ctx).Errorf("[Client-%d] Failed to set read deadline: %v", slotID, err)
				util.SafeWrite(errChan, fmt.Errorf("failed to set read deadline: %w", err))
				return
			}
		}
		n, err := conn.Read(buf[:])
		if err != nil {
			config.LPool.Put(buf[:])
			plog.G(ctx).Errorf("[Client-%d] Failed to read from remote: %v", slotID, err)
			util.SafeWrite(errChan, fmt.Errorf("failed to read packet from remote %s: %w", conn.RemoteAddr(), err))
			return
		}
		if n < 1 {
			config.LPool.Put(buf[:])
			continue
		}
		if buf[0] == 1 {
			gvisorInbound <- NewPacket(buf[:], n, nil, nil)
		} else {
			tunOutbound <- NewPacket(buf[:], n, nil, nil)
		}
	}
}

// writeToConn writes packets from a slot's inbound channel to the raw TCP conn with datagram framing.
func writeToConn(ctx context.Context, rawConn net.Conn, inbound <-chan *Packet, errChan chan error, slotID int) {
	defer util.HandleCrash()
	writeTimeout := config.KeepAliveTime
	nextDeadline := time.Now().Add(writeTimeout)
	_ = rawConn.SetWriteDeadline(nextDeadline)
	for {
		select {
		case packet := <-inbound:
			if packet == nil {
				return
			}
			if time.Until(nextDeadline) < writeTimeout/2 {
				nextDeadline = time.Now().Add(writeTimeout)
				if err := rawConn.SetWriteDeadline(nextDeadline); err != nil {
					plog.G(ctx).Errorf("[Client-%d] Failed to set write deadline: %v", slotID, err)
					util.SafeWrite(errChan, fmt.Errorf("failed to set write deadline: %w", err))
					return
				}
			}
			// Write datagram frame in-place: [2-byte length header][prefix+IP data]
			binary.BigEndian.PutUint16(packet.data[:2], uint16(packet.length))
			_, err := rawConn.Write(packet.data[:packet.length+2])
			config.LPool.Put(packet.data[:])
			if err != nil {
				plog.G(ctx).Errorf("[Client-%d] Failed to write to remote: %v", slotID, err)
				util.SafeWrite(errChan, fmt.Errorf("failed to write packet to remote: %w", err))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *ClientDevice) readFromTun(ctx context.Context) {
	defer util.HandleCrash()
	gvisorInbound := make(chan *Packet, MaxSize)
	go handleGvisorPacket(gvisorInbound, d.tunOutbound, 0).Run(ctx)
	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		// Read at offset 3 to reserve [2-byte datagram header][1-byte prefix].
		n, err := d.tun.Read(buf[3:])
		if err != nil {
			plog.G(ctx).Errorf("[Client] Failed to read from TUN: %v", err)
			util.SafeWrite(d.errChan, err)
			config.LPool.Put(buf[:])
			return
		}
		buf[2] = 1

		src, dst, protocol, parseErr := util.ParseIPFast(buf[3 : 3+n])
		if parseErr != nil {
			plog.G(ctx).Errorf("[Client] Unknown packet, dropping: %v", parseErr)
			config.LPool.Put(buf[:])
			continue
		}
		if config.Debug {
			plog.G(ctx).Debugf("[Client] SRC: %s, DST: %s, Protocol: %s, Length: %d", src, dst, layers.IPProtocol(protocol).String(), n)
		}
		if src.Equal(dst) {
			copy(buf[0:n+1], buf[2:3+n])
			gvisorInbound <- NewPacket(buf[:], n+1, src, dst)
		} else {
			// Send to shared tunInbound; runConnPool distributes to slot by hash(dst)
			d.tunInbound <- NewPacket(buf[:], n+1, src, dst)
		}
	}
}

func (d *ClientDevice) heartbeats(ctx context.Context) {
	tunIfi, err := util.GetTunDeviceByConn(d.tun)
	if err != nil {
		plog.G(ctx).Errorf("[Client] Failed to get tun device: %v", err)
		return
	}

	ticker := time.NewTicker(config.KeepAliveTime)
	defer ticker.Stop()

	sendHeartbeat := func(payload []byte) {
		buf := config.LPool.Get().([]byte)
		n := copy(buf[3:], payload)
		buf[2] = 1
		d.tunInbound <- &Packet{data: buf, length: n + 1}
	}

	sendAll := func(reason string) {
		srcIPv4, srcIPv6, dockerSrcIPv4, _ := util.GetTunDeviceIP(tunIfi.Name)
		plog.G(ctx).Debugf("[Client] Sending heartbeat (%s)", reason)
		if srcIPv4 != nil {
			if icmp, e := util.GenICMPPacket(srcIPv4, config.RouterIP); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv4 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if srcIPv6 != nil {
			if icmp, e := util.GenICMPPacketIPv6(srcIPv6, config.RouterIP6); e != nil {
				plog.G(ctx).Errorf("[Client] Failed to generate IPv6 heartbeat: %v", e)
			} else {
				sendHeartbeat(icmp)
			}
		}
		if dockerSrcIPv4 != nil {
			_, _ = util.Ping(ctx, dockerSrcIPv4.String(), config.DockerRouterIP.String())
		}
	}

	sendAll("initial")
	for ctx.Err() == nil {
		select {
		case <-ticker.C:
			sendAll("periodic")
		case <-d.reconnected:
			sendAll("reconnected")
			ticker.Reset(config.KeepAliveTime)
		case <-ctx.Done():
			return
		}
	}
}
