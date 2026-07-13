package core

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// TestInterClient_ICMP_EndToEnd reproduces "client A pings client B's TUN IP" entirely in
// memory (no real TUN, no cluster): two clientTransports connect to one server over real
// localhost TCP, and a net.Pipe stands in for each client's TUN device. It asserts that an
// ICMP echo request A->B produces a *valid* echo reply delivered back to A's TUN — the exact
// path a `ping 198.18.0.3` from 198.18.0.5 exercises, for both IPv4 and IPv6.
//
// Crucially it validates the reply's ICMP checksum: a real OS ping silently drops a reply with
// a bad checksum, so a test that only checks addressing/type would pass while `ping` still
// fails. The only pre-existing inter-client ping test (TestTUN_InterClient_Routing) is behind
// the `tun` build tag AND treats ping failure as a warning, so this scenario has never actually
// been asserted in a runnable test.
func TestInterClient_ICMP_EndToEnd(t *testing.T) {
	cases := []struct {
		name           string
		pinger, pingee string
	}{
		{"ipv4", "198.18.0.5", "198.18.0.3"},
		{"ipv6", "2001:2::5", "2001:2::3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			hub := newInterClientServer(ctx, t)
			serverPort := hub.port

			osA, replyA := startPipeClient(ctx, t, serverPort, tc.pinger)
			osB, _ := startPipeClient(ctx, t, serverPort, tc.pingee)

			// Register both routes on the server by having each client emit a packet with its own
			// src IP (mirrors the production route-registration echo to the gateway).
			registerRoute(t, osA, tc.pinger)
			registerRoute(t, osB, tc.pingee)
			waitForRoutes(t, hub.hub, tc.pinger, tc.pingee)

			// A pings B.
			req := genEchoRequest(t, tc.pinger, tc.pingee)
			if _, err := osA.Write(req); err != nil {
				t.Fatalf("write ping into A's TUN: %v", err)
			}

			select {
			case pkt := <-replyA:
				src, dst, _, perr := netutil.ParseIPFast(pkt)
				if perr != nil {
					t.Fatalf("parse reply: %v", perr)
				}
				t.Logf("A received echo reply: SRC=%s DST=%s", src, dst)
				if !src.Equal(net.ParseIP(tc.pingee)) || !dst.Equal(net.ParseIP(tc.pinger)) {
					t.Fatalf("reply addressing = %s->%s, want %s->%s", src, dst, tc.pingee, tc.pinger)
				}
				// A real OS ping validates the ICMP checksum and drops the reply if it is wrong.
				if err := verifyEchoReplyChecksum(pkt); err != nil {
					t.Fatalf("reply has invalid checksum (OS ping would drop it): %v", err)
				}
				t.Logf("✅ inter-client %s ping succeeded with a valid checksum", tc.name)
			case <-time.After(10 * time.Second):
				t.Fatalf("❌ no ICMP echo reply reached pinger %s — inter-client ping broken", tc.pinger)
			}
		})
	}
}

// TestInterClient_ICMP_ServerAnswersWhenPeerRouteMissing covers the fallback path: when the
// pingee has no registered route on the server (idle peer, or its route was evicted), the
// server must answer the echo request from its own gvisor stack (via spoofing) so the pinger
// still gets a reply. This is the branch that diverges from the peer-to-peer path in
// readFromTCPConnWriteToEndpoint (HasRoute miss -> InjectInbound), and a real ping depends on
// it whenever the peer's route is not currently present.
func TestInterClient_ICMP_ServerAnswersWhenPeerRouteMissing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hub := newInterClientServer(ctx, t)
	osA, replyA := startPipeClient(ctx, t, hub.port, "198.18.0.5")

	// Only A registers; the pingee 198.18.0.3 is never announced, so HasRoute(.3) is false.
	registerRoute(t, osA, "198.18.0.5")
	waitForRoutes(t, hub.hub, "198.18.0.5")

	if _, err := osA.Write(genEchoRequest(t, "198.18.0.5", "198.18.0.3")); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	select {
	case pkt := <-replyA:
		src, dst, _, _ := netutil.ParseIPFast(pkt)
		if !src.Equal(net.ParseIP("198.18.0.3")) || !dst.Equal(net.ParseIP("198.18.0.5")) {
			t.Fatalf("fallback reply addressing = %s->%s, want 198.18.0.3->198.18.0.5", src, dst)
		}
		if err := verifyEchoReplyChecksum(pkt); err != nil {
			t.Fatalf("fallback reply invalid checksum: %v", err)
		}
		t.Log("✅ server answered ping on behalf of the unregistered peer")
	case <-time.After(10 * time.Second):
		t.Fatal("❌ no reply when peer route missing — server fallback (spoofed FindRoute) broken")
	}
}

type interClientServer struct {
	hub  *RouteHub
	port int
}

func newInterClientServer(ctx context.Context, t *testing.T) interClientServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server listener: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	hub := NewRouteHub()
	handler := GvisorLocalTCPHandler(hub)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler.Handle(ctx, conn)
		}
	}()
	return interClientServer{hub: hub, port: ln.Addr().(*net.TCPAddr).Port}
}

// startPipeClient builds a clientTransport whose TUN is a net.Pipe. It returns the "OS side" of
// the pipe (write to it = the OS sending a packet out the TUN; read from it = the OS receiving a
// packet) and a channel of ICMP/ICMPv6 echo replies addressed to this client's TUN IP.
func startPipeClient(ctx context.Context, t *testing.T, serverPort int, tunIP string) (net.Conn, <-chan []byte) {
	t.Helper()
	osSide, tunSide := net.Pipe()

	forwarder := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}

	device := &tunDevice{
		tun:         tunSide,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	// Non-nil stats so the client drops gateway heartbeat echo replies (matching production),
	// keeping the route-registration reply from polluting the observed ping replies.
	device.transport = newClientTransport(device, forwarder, &HeartbeatStats{})
	for _, r := range device.routines() {
		go r.fn(ctx)
	}
	t.Cleanup(func() { device.Close() })

	// Drain the OS side of the pipe continuously (writeToTun blocks on an unbuffered net.Pipe
	// until the OS reads), surfacing echo replies addressed to this client.
	replies := make(chan []byte, 16)
	go func() {
		self := net.ParseIP(tunIP)
		buf := make([]byte, config.LargeBufferSize)
		for ctx.Err() == nil {
			n, err := osSide.Read(buf)
			if err != nil {
				return
			}
			ipData := make([]byte, n)
			copy(ipData, buf[:n])
			_, dst, _, perr := netutil.ParseIPFast(ipData)
			if perr != nil || !dst.Equal(self) {
				continue
			}
			if isEchoReply(ipData) {
				select {
				case replies <- ipData:
				default:
				}
			}
		}
	}()
	return osSide, replies
}

// registerRoute makes the server learn this client's route by emitting a heartbeat-style ICMP
// echo to the gateway (src = the client's TUN IP), which the server AddRoutes on receipt.
func registerRoute(t *testing.T, os net.Conn, tunIP string) {
	t.Helper()
	ip := net.ParseIP(tunIP)
	var pkt []byte
	var err error
	if ip.To4() != nil {
		pkt, err = netutil.GenICMPPacket(ip.To4(), config.RouterIP.To4())
	} else {
		pkt, err = netutil.GenICMPPacketIPv6(ip.To16(), config.RouterIP6.To16())
	}
	if err != nil {
		t.Fatalf("gen registration packet: %v", err)
	}
	if _, err := os.Write(pkt); err != nil {
		t.Fatalf("write registration packet: %v", err)
	}
}

// genEchoRequest builds a well-formed ICMP/ICMPv6 echo request (with a proper id/seq body),
// mimicking what a real OS `ping` emits — NOT the kubevpn heartbeat helpers, so a datapath bug
// is not masked by a malformed generator.
func genEchoRequest(t *testing.T, src, dst string) []byte {
	t.Helper()
	srcIP, dstIP := net.ParseIP(src), net.ParseIP(dst)
	const ident, seq = 0x1234, 0x0001
	payload := []byte("pingpong")
	if srcIP.To4() != nil {
		req := buildICMPv4EchoRequest(tcpip.AddrFrom4([4]byte(srcIP.To4())), tcpip.AddrFrom4([4]byte(dstIP.To4())), ident, seq, payload)
		defer req.DecRef()
		return req.Data().AsRange().ToSlice()
	}
	req := buildICMPv6EchoRequest(tcpip.AddrFrom16([16]byte(srcIP.To16())), tcpip.AddrFrom16([16]byte(dstIP.To16())), ident, seq, payload)
	defer req.DecRef()
	return req.Data().AsRange().ToSlice()
}

// isEchoReply reports whether ipData is an ICMPv4 (type 0) or ICMPv6 (type 129) echo reply.
func isEchoReply(ipData []byte) bool {
	if netutil.IsIPv4(ipData) {
		ihl := int(ipData[0]&0x0f) * 4
		return ipData[9] == 1 && len(ipData) > ihl && ipData[ihl] == 0
	}
	if netutil.IsIPv6(ipData) {
		const l = 40
		return ipData[6] == 58 && len(ipData) > l && ipData[l] == 129
	}
	return false
}

// verifyEchoReplyChecksum recomputes the ICMP/ICMPv6 checksum of the reply and compares it to
// the stored value — exactly the validation a receiving OS performs before accepting a ping
// reply. A mismatch means a real `ping` would silently discard the packet.
func verifyEchoReplyChecksum(ipData []byte) error {
	if netutil.IsIPv4(ipData) {
		ihl := int(ipData[0]&0x0f) * 4
		icmp := header.ICMPv4(ipData[ihl:])
		stored := icmp.Checksum()
		icmp.SetChecksum(0)
		want := header.ICMPv4Checksum(icmp, 0)
		icmp.SetChecksum(stored)
		if stored != want {
			return fmt.Errorf("ICMPv4 checksum stored=%#x want=%#x", stored, want)
		}
		return nil
	}
	if netutil.IsIPv6(ipData) {
		const l = 40
		icmp := header.ICMPv6(ipData[l:])
		stored := icmp.Checksum()
		icmp.SetChecksum(0)
		want := header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
			Header: icmp,
			Src:    tcpip.AddrFrom16([16]byte(ipData[8:24])),
			Dst:    tcpip.AddrFrom16([16]byte(ipData[24:40])),
		})
		icmp.SetChecksum(stored)
		if stored != want {
			return fmt.Errorf("ICMPv6 checksum stored=%#x want=%#x", stored, want)
		}
		return nil
	}
	return fmt.Errorf("not an IP packet")
}

func waitForRoutes(t *testing.T, hub *RouteHub, ips ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		var key string
		if parsed.To4() != nil {
			key = string(parsed.To4())
		} else {
			key = string(parsed.To16())
		}
		for !hub.HasRoute(key) {
			if time.Now().After(deadline) {
				t.Fatalf("route for %s not registered in time", ip)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
