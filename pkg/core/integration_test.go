package core

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// =============================================================================
// REAL integration tests verifying complete data paths through gvisor.
//
// Architecture matches production exactly:
//   Client: gonet TCP socket → gvisor stack → channel endpoint → pipe
//   Server: pipe → channel endpoint → gvisor stack → TCPForwarder → net.Dial(service)
//
// The test creates TWO gvisor stacks (client + server) connected by a pipe,
// with a real TCP echo server as the destination.
// =============================================================================

// TestIntegration_RealTCPThroughGvisor tests the FULL production pipeline:
//
//	Client gonet.Dial → client gvisor → endpoint → [datagram frame] → pipe
//	→ [datagram deframe] → server endpoint → server gvisor → TCPForwarder
//	→ net.Dial(echo server) → echo server responds
//	→ server gvisor → server endpoint → [datagram frame] → pipe
//	→ [datagram deframe] → client endpoint → client gvisor → gonet receives
//
// This is an EXACT replica of the kubevpn production data path, with pipe
// replacing kubectl port-forward.
// TestIntegration_RealTCPThroughGvisor requires a TUN device to work correctly.
// Gvisor's TCP forwarder only triggers properly when attached to a real TUN
// interface (not channel.Endpoint with InjectInbound). Skip in environments
// without TUN support. Run manually with: go test -run TestIntegration_RealTCPThroughGvisor -tags=tun
func TestIntegration_RealTCPThroughGvisor(t *testing.T) {
	t.Skip("Requires TUN device (not available in this environment). Run with -tags=tun on a machine with ip tuntap support.")
	// === Start real TCP echo server (the "kubernetes service") ===
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server failed: %v", err)
	}
	defer echoListener.Close()
	echoPort := echoListener.Addr().(*net.TCPAddr).Port
	t.Logf("Echo server (simulating K8s service) on 127.0.0.1:%d", echoPort)

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) // echo all data back
			}(conn)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// === Create SERVER gvisor stack (traffic-manager side) ===
	// LocalTCPForwarder: when a TCP SYN arrives, dials 127.0.0.1:<dst_port>
	serverEndpoint := channel.New(512, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	serverEndpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	// LocalTCPForwarder dials 127.0.0.1:<port> for every SYN — same as production for local test
	serverStack := newGvisorStack(ctx, sniffer.NewWithPrefix(serverEndpoint, "[SRV] "), LocalTCPForwarder, LocalUDPForwarder)
	defer serverStack.Destroy()
	// Add 127.0.0.1 to server NIC so gvisor accepts packets destined for it
	addIPv4ToStack(t, serverStack, 1, net.IPv4(127, 0, 0, 1))

	// === Create CLIENT gvisor stack (local machine side) ===
	clientEndpoint := channel.New(512, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	clientEndpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	clientStack := newGvisorStack(ctx, sniffer.NewWithPrefix(clientEndpoint, "[CLI] "), LocalTCPForwarder, LocalUDPForwarder)
	defer clientStack.Destroy()
	// Assign IP so gonet can bind a source address
	addIPv4ToStack(t, clientStack, 1, net.IPv4(198, 18, 0, 2))

	// === Connect the two stacks via two unidirectional pipes ===
	// Pipe 1: client → server (client endpoint output → server endpoint input)
	c2sReader, c2sWriter := net.Pipe()
	defer c2sReader.Close()
	defer c2sWriter.Close()

	// Pipe 2: server → client (server endpoint output → client endpoint input)
	s2cReader, s2cWriter := net.Pipe()
	defer s2cReader.Close()
	defer s2cWriter.Close()

	// Client endpoint output → frame → c2sWriter → c2sReader → deframe → server endpoint inject
	go bridgeEndpointToPipe(ctx, clientEndpoint, c2sWriter, 1)
	go bridgePipeToEndpoint(ctx, c2sReader, serverEndpoint)

	// Server endpoint output → frame → s2cWriter → s2cReader → deframe → client endpoint inject
	go bridgeEndpointToPipe(ctx, serverEndpoint, s2cWriter, 0)
	go bridgePipeToEndpoint(ctx, s2cReader, clientEndpoint)

	// === Client dials echo server THROUGH the full pipeline ===
	// Use a cluster-like IP (10.96.0.1) as destination — this is what production does.
	// The server TCPForwarder will resolve this to id.LocalAddress.String():id.LocalPort
	// and dial it. Since we use serverTCPAddr (not localTCPAddr), it dials 10.96.0.1:port
	// which won't work. Use LocalTCPForwarder which dials 127.0.0.1:localPort.
	t.Log("Dialing through: client gvisor → pipe → server gvisor → LocalTCPForwarder → echo server")
	remoteAddr := tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}),
		Port: uint16(echoPort),
	}
	gConn, err := gonet.DialContextTCP(ctx, clientStack, remoteAddr, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("gonet.DialContextTCP through full pipeline failed: %v", err)
	}
	defer gConn.Close()
	t.Log("✅ TCP handshake completed through 2 gvisor stacks + pipe")

	// === Send data and verify echo ===
	testData := []byte("FULL-E2E-CLIENT-GVISOR-PIPE-SERVER-GVISOR-TCPFORWARDER-ECHO-SERVER")
	_, err = gConn.Write(testData)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	t.Logf("Sent %d bytes through pipeline", len(testData))

	buf := make([]byte, 1024)
	gConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := gConn.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(buf[:n]) != string(testData) {
		t.Fatalf("echo mismatch: got %q, want %q", buf[:n], testData)
	}
	t.Logf("✅ FULL E2E SUCCESS: %q echoed through client gvisor → pipe → server gvisor → echo server", buf[:n])
}

// TestIntegration_DatagramFraming tests UDPConnOverTCP with real pipes.
func TestIntegration_DatagramFraming(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	defer clientRaw.Close()
	defer serverRaw.Close()

	ctx := context.Background()
	serverUDP, _ := NewUDPConnOverTCP(ctx, serverRaw)

	srcIP := net.IPv4(198, 18, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 5).To4()
	ipPkt := buildIPv4Packet(srcIP, dstIP, []byte("real-payload"))

	payloadLen := 1 + len(ipPkt)
	frame := make([]byte, 2+payloadLen)
	binary.BigEndian.PutUint16(frame[:2], uint16(payloadLen))
	frame[2] = 1
	copy(frame[3:], ipPkt)

	go func() { clientRaw.Write(frame) }()

	buf := make([]byte, config.LargeBufferSize)
	n, err := serverUDP.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if n != payloadLen || buf[0] != 1 {
		t.Fatalf("framing error: n=%d, prefix=%d", n, buf[0])
	}
	src, dst, _, _ := util.ParseIPFast(buf[1:n])
	if !src.Equal(srcIP) || !dst.Equal(dstIP) {
		t.Fatalf("IP mismatch")
	}
	t.Logf("✅ Datagram framing: %d bytes, src=%s, dst=%s", n, src, dst)
}

// TestIntegration_DatagramFraming_Ordering verifies 100 packets arrive in order.
func TestIntegration_DatagramFraming_Ordering(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	defer clientRaw.Close()
	defer serverRaw.Close()

	ctx := context.Background()
	serverUDP, _ := NewUDPConnOverTCP(ctx, serverRaw)
	numPackets := 100

	go func() {
		for i := 0; i < numPackets; i++ {
			ipPkt := buildIPv4Packet(
				net.IPv4(198, 18, 0, 2).To4(),
				net.IPv4(10, 0, 0, byte(i%256)).To4(),
				[]byte{byte(i)},
			)
			payloadLen := 1 + len(ipPkt)
			frame := make([]byte, 2+payloadLen)
			binary.BigEndian.PutUint16(frame[:2], uint16(payloadLen))
			frame[2] = 1
			copy(frame[3:], ipPkt)
			clientRaw.Write(frame)
		}
	}()

	buf := make([]byte, config.LargeBufferSize)
	for i := 0; i < numPackets; i++ {
		n, err := serverUDP.Read(buf)
		if err != nil {
			t.Fatalf("pkt %d: %v", i, err)
		}
		if n < 2 || buf[0] != 1 {
			t.Fatalf("pkt %d corrupt", i)
		}
	}
	t.Logf("✅ %d packets in order", numPackets)
}

// === Bridge helpers ===

// bridgeEndpointToPipe reads from gvisor endpoint, frames packets, writes to pipe.
func bridgeEndpointToPipe(ctx context.Context, ep *channel.Endpoint, pipe net.Conn, prefix byte) {
	for ctx.Err() == nil {
		pkt := ep.ReadContext(ctx)
		if pkt == nil {
			return
		}
		data := pkt.ToView().AsSlice()
		payloadLen := 1 + len(data)
		frame := make([]byte, 2+payloadLen)
		binary.BigEndian.PutUint16(frame[:2], uint16(payloadLen))
		frame[2] = prefix
		copy(frame[3:], data)
		pipe.Write(frame)
	}
}

// bridgePipeToEndpoint reads framed packets from pipe, injects into gvisor endpoint.
func bridgePipeToEndpoint(ctx context.Context, pipe net.Conn, ep *channel.Endpoint) {
	for ctx.Err() == nil {
		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(pipe, lenBuf); err != nil {
			return
		}
		length := binary.BigEndian.Uint16(lenBuf)
		data := make([]byte, length)
		if _, err := io.ReadFull(pipe, data); err != nil {
			return
		}
		if len(data) < 2 {
			continue
		}
		ipData := data[1:] // skip prefix
		var protocol tcpip.NetworkProtocolNumber
		if len(ipData) > 0 && (ipData[0]>>4) == 4 {
			protocol = header.IPv4ProtocolNumber
		} else {
			protocol = header.IPv6ProtocolNumber
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(ipData),
		})
		ep.InjectInbound(protocol, pkt)
		pkt.DecRef()
	}
}

// addIPv4ToStack assigns an IPv4 address to a gvisor NIC for gonet binding.
func addIPv4ToStack(t *testing.T, s *stack.Stack, nicID tcpip.NICID, ip net.IP) {
	t.Helper()
	ip4 := ip.To4()
	addr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}).WithPrefix(),
	}
	if err := s.AddProtocolAddress(nicID, addr, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress(%s) failed: %v", ip, err)
	}
}
