package core

import (
	"context"
	"net"

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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func NewTunEndpoint(ctx context.Context, tun net.Conn, mtu uint32, engine config.Engine, in chan<- *DataElem, out chan *DataElem) stack.LinkEndpoint {
	addr, _ := tcpip.ParseMACAddress("02:03:03:04:05:06")
	endpoint := channel.New(tcp.DefaultReceiveBufferSize, mtu, addr)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			read := endpoint.ReadContext(ctx)
			if read != nil {
				bb := read.ToView().AsSlice()
				i := config.LPool.Get().([]byte)[:]
				n := copy(i, bb)
				bb = nil
				out <- NewDataElem(i[:], n, nil, nil)
			}
		}
	}()
	// tun --> dispatcher
	go func() {
		// full(all use gvisor), mix(cluster network use gvisor), raw(not use gvisor)
		for {
			bytes := config.LPool.Get().([]byte)[:]
			read, err := tun.Read(bytes[:])
			if err != nil {
				// if context is still going
				if ctx.Err() == nil {
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
					log.Errorf("parse ipv4 header failed: %s", err.Error())
					continue
				}
				ipProtocol = ipHeader.Protocol
				src = ipHeader.Src
				dst = ipHeader.Dst
			} else if version == 6 {
				protocol = header.IPv6ProtocolNumber
				ipHeader, err := ipv6.ParseHeader(bytes[:read])
				if err != nil {
					log.Errorf("parse ipv6 header failed: %s", err.Error())
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
				(engine == config.EngineGvisor || (engine == config.EngineMix && (!config.CIDR.Contains(dst) && !config.CIDR6.Contains(dst)))) {
				pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
					ReserveHeaderBytes: 0,
					Payload:            buffer.MakeWithData(bytes[:read]),
				})
				//defer pkt.DecRef()
				config.LPool.Put(bytes[:])
				endpoint.InjectInbound(protocol, pkt)
				log.Debugf("[TUN-%s] IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
			} else {
				log.Debugf("[TUN-RAW] IP-Protocol: %s, SRC: %s, DST: %s, Length: %d", layers.IPProtocol(ipProtocol).String(), src.String(), dst, read)
				in <- NewDataElem(bytes[:], read, src, dst)
			}
		}
	}()
	go func() {
		for elem := range out {
			_, err := tun.Write(elem.Data()[:elem.Length()])
			config.LPool.Put(elem.Data()[:])
			if err != nil {
				log.Fatalf("[TUN] Fatal: failed to write data to tun device: %v", err)
			}
		}
	}()
	return endpoint
}
