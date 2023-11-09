package core

import (
	"context"
	"net"
	"sync"

	"github.com/google/gopacket/layers"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

var _ stack.LinkEndpoint = (*tunEndpoint)(nil)

// tunEndpoint /Users/naison/go/pkg/mod/gvisor.dev/gvisor@v0.0.0-20220422052705-39790bd3a15a/pkg/tcpip/link/tun/device.go:122
type tunEndpoint struct {
	ctx      context.Context
	tun      net.Conn
	once     sync.Once
	endpoint *channel.Endpoint
	engine   config.Engine

	in  chan<- *DataElem
	out chan *DataElem
}

// WritePackets writes packets. Must not be called with an empty list of
// packet buffers.
//
// WritePackets may modify the packet buffers, and takes ownership of the PacketBufferList.
// it is not safe to use the PacketBufferList after a call to WritePackets.
func (e *tunEndpoint) WritePackets(p stack.PacketBufferList) (int, tcpip.Error) {
	return e.endpoint.WritePackets(p)
}

// MTU is the maximum transmission unit for this endpoint. This is
// usually dictated by the backing physical network; when such a
// physical network doesn't exist, the limit is generally 64k, which
// includes the maximum size of an IP packet.
func (e *tunEndpoint) MTU() uint32 {
	return uint32(config.DefaultMTU)
}

// MaxHeaderLength returns the maximum size the data link (and
// lower level layers combined) headers can have. Higher levels use this
// information to reserve space in the front of the packets they're
// building.
func (e *tunEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link address (typically a MAC) of the
// endpoint.
func (e *tunEndpoint) LinkAddress() tcpip.LinkAddress {
	return e.endpoint.LinkAddress()
}

// Capabilities returns the set of capabilities supported by the
// endpoint.
func (e *tunEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return e.endpoint.LinkEPCapabilities
}

// Attach attaches the data link layer endpoint to the network-layer
// dispatcher of the stack.
//
// Attach is called with a nil dispatcher when the endpoint's NIC is being
// removed.
func (e *tunEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.endpoint.Attach(dispatcher)
	// queue --> tun
	e.once.Do(func() {
		go func() {
			for {
				select {
				case <-e.ctx.Done():
					return
				default:
				}
				read := e.endpoint.ReadContext(e.ctx)
				if !read.IsNil() {
					bb := read.ToView().AsSlice()
					i := config.LPool.Get().([]byte)[:]
					n := copy(i, bb)
					bb = nil
					e.out <- NewDataElem(i[:], n, nil, nil)
				}
			}
		}()
		// tun --> dispatcher
		go func() {
			// full(all use gvisor), mix(cluster network use gvisor), raw(not use gvisor)
			for {
				bytes := config.LPool.Get().([]byte)[:]
				read, err := e.tun.Read(bytes[:])
				if err != nil {
					// if context is still going
					if e.ctx.Err() == nil {
						log.Fatalf("[TUN]: read from tun failed: %v", err)
					} else {
						log.Info("tun device closed")
					}
					return
				}
				if read == 0 {
					log.Warnf("[TUN]: read from tun length is %d", read)
					continue
				}
				// Try to determine network protocol number, default zero.
				var protocol tcpip.NetworkProtocolNumber
				var ipProtocol int
				var src, dst net.IP
				// TUN interface with IFF_NO_PI enabled, thus
				// we need to determine protocol from version field
				version := bytes[0] >> 4
				if version == 4 {
					protocol = header.IPv4ProtocolNumber
					ipHeader, err := ipv4.ParseHeader(bytes[:read])
					if err != nil {
						errors.LogErrorf("parse ipv4 header failed: %s", err.Error())
						continue
					}
					ipProtocol = ipHeader.Protocol
					src = ipHeader.Src
					dst = ipHeader.Dst
				} else if version == 6 {
					protocol = header.IPv6ProtocolNumber
					ipHeader, err := ipv6.ParseHeader(bytes[:read])
					if err != nil {
						errors.LogErrorf("parse ipv6 header failed: %s", err.Error())
						continue
					}
					ipProtocol = ipHeader.NextHeader
					src = ipHeader.Src
					dst = ipHeader.Dst
				} else {
					log.Debugf("[TUN-gvisor] unknown packet version %d", version)
					continue
				}
				// only tcp and udp needs to distinguish transport engine
				//   gvisor: all network use gvisor
				//   mix: cluster network use gvisor, diy network use raw
				//   raw: all network use raw
				if (ipProtocol == int(layers.IPProtocolUDP) || ipProtocol == int(layers.IPProtocolUDPLite) || ipProtocol == int(layers.IPProtocolTCP)) &&
					(e.engine == config.EngineGvisor || (e.engine == config.EngineMix && (!config.CIDR.Contains(dst) && !config.CIDR6.Contains(dst)))) {
					pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
						ReserveHeaderBytes: 0,
						Payload:            buffer.MakeWithData(bytes[:read]),
					})
					//defer pkt.DecRef()
					config.LPool.Put(bytes[:])
					e.endpoint.InjectInbound(protocol, pkt)
					log.Debugf("[TUN-%s] IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
				} else {
					log.Debugf("[TUN-RAW] IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
					e.in <- NewDataElem(bytes[:], read, src, dst)
				}
			}
		}()
		go func() {
			for elem := range e.out {
				_, err := e.tun.Write(elem.Data()[:elem.Length()])
				config.LPool.Put(elem.Data()[:])
				if err != nil {
					log.Fatalf("[TUN] Fatal: failed to write data to tun device: %v", err)
				}
			}
		}()
	})
}

// IsAttached returns whether a NetworkDispatcher is attached to the
// endpoint.
func (e *tunEndpoint) IsAttached() bool {
	return e.endpoint.IsAttached()
}

// Wait waits for any worker goroutines owned by the endpoint to stop.
//
// For now, requesting that an endpoint's worker goroutine(s) stop is
// implementation specific.
//
// Wait will not block if the endpoint hasn't started any goroutines
// yet, even if it might later.
func (e *tunEndpoint) Wait() {
	return
}

// ARPHardwareType returns the ARPHRD_TYPE of the link endpoint.
//
// See:
// https://github.com/torvalds/linux/blob/aa0c9086b40c17a7ad94425b3b70dd1fdd7497bf/include/uapi/linux/if_arp.h#L30
func (e *tunEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader adds a link layer header to the packet if required.
func (e *tunEndpoint) AddHeader(ptr stack.PacketBufferPtr) {
	return
}

func NewTunEndpoint(ctx context.Context, tun net.Conn, mtu uint32, engine config.Engine, in chan<- *DataElem, out chan *DataElem) stack.LinkEndpoint {
	addr, _ := tcpip.ParseMACAddress("02:03:03:04:05:06")
	return &tunEndpoint{
		ctx:      ctx,
		tun:      tun,
		endpoint: channel.New(tcp.DefaultReceiveBufferSize, mtu, addr),
		engine:   engine,
		in:       in,
		out:      out,
	}
}
