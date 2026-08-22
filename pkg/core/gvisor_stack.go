package core

import (
	"context"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/packetsocket"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type transportForwarder func(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool

func newGvisorStack(ctx context.Context, tun stack.LinkEndpoint, tcpFwd, udpFwd transportForwarder) *stack.Stack {
	nicID := tcpip.NICID(1)
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
		},
		Clock:                    tcpip.NewStdClock(),
		AllowPacketEndpointWrite: true,
		HandleLocal:              false,
		RawFactory:               raw.EndpointFactory{},
	})

	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd(ctx, s))
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd(ctx, s))
	s.SetTransportProtocolHandler(header.ICMPv4ProtocolNumber, ICMPForwarder(ctx, s))
	s.SetTransportProtocolHandler(header.ICMPv6ProtocolNumber, ICMPForwarder(ctx, s))

	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	s.CreateNICWithOptions(nicID, packetsocket.New(tun), stack.NICOptions{
		Disabled: false,
		Context:  ctx,
	})
	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)

	{
		opt := tcpip.TCPSACKEnabled(true)
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt); err != nil {
			plog.G(ctx).Fatalf("SetTransportProtocolOption(%d, &%T(%t)): %v", tcp.ProtocolNumber, opt, opt, err)
		}
	}

	{
		opt := tcpip.DefaultTTLOption(64)
		if err := s.SetNetworkProtocolOption(ipv4.ProtocolNumber, &opt); err != nil {
			plog.G(ctx).Fatalf("SetNetworkProtocolOption(%d, &%T(%d)): %v", ipv4.ProtocolNumber, opt, opt, err)
		}
		if err := s.SetNetworkProtocolOption(ipv6.ProtocolNumber, &opt); err != nil {
			plog.G(ctx).Fatalf("SetNetworkProtocolOption(%d, &%T(%d)): %v", ipv6.ProtocolNumber, opt, opt, err)
		}
	}

	{
		opt := tcpip.TCPModerateReceiveBufferOption(true)
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt); err != nil {
			plog.G(ctx).Fatalf("SetTransportProtocolOption(%d, &%T(%t)): %v", tcp.ProtocolNumber, opt, opt, err)
		}
	}

	// Loosen the TCP send/receive buffer caps well above gvisor's defaults so the advertised
	// window can grow on high bandwidth-delay-product paths (the server stack forwards every
	// client's cluster traffic). Mirrors Tailscale's netstack tuning (RX 8MiB / TX 6MiB); the
	// autotuner (TCPModerateReceiveBufferOption above) scales within these bounds.
	{
		rcv := tcpip.TCPReceiveBufferSizeRangeOption{
			Min:     tcp.MinBufferSize,
			Default: tcp.DefaultReceiveBufferSize,
			Max:     8 << 20,
		}
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv); err != nil {
			plog.G(ctx).Fatalf("SetTransportProtocolOption(%d, &%T): %v", tcp.ProtocolNumber, rcv, err)
		}
		snd := tcpip.TCPSendBufferSizeRangeOption{
			Min:     tcp.MinBufferSize,
			Default: tcp.DefaultSendBufferSize,
			Max:     6 << 20,
		}
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd); err != nil {
			plog.G(ctx).Fatalf("SetTransportProtocolOption(%d, &%T): %v", tcp.ProtocolNumber, snd, err)
		}
	}

	{
		if err := s.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, true); err != nil {
			plog.G(ctx).Fatalf("Set IPv4 forwarding: %v", err)
		}
		if err := s.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, true); err != nil {
			plog.G(ctx).Fatalf("Set IPv6 forwarding: %v", err)
		}
	}

	return s
}

// NewStack creates a gvisor network stack with TCP/UDP/ICMP forwarding to real destinations.
func NewStack(ctx context.Context, tun stack.LinkEndpoint) *stack.Stack {
	return newGvisorStack(ctx, tun, TCPForwarder, UDPForwarder)
}

// NewLocalStack creates a gvisor stack that forwards to localhost (for testing and dev mode).
func NewLocalStack(ctx context.Context, tun stack.LinkEndpoint) *stack.Stack {
	return newGvisorStack(ctx, tun, LocalTCPForwarder, LocalUDPForwarder)
}
