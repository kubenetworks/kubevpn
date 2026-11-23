package tunnel

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// Tests for the two guards that break the route-table self-registration loop (the bug
// that black-holed every client flow to the traffic manager's own ClusterIP — DNS :53 and
// xDS :9002 — in SSH mode):
//  1. the route table must never learn a route for a hub-owned IP (the manager ClusterIP);
//  2. a hub-owned destination must be handled by the local gvisor forwarder even if a
//     (stale/bogus) route for it exists;
//  3. the client must not feed foreign destinations into its promiscuous inter-client
//     stack, whose spoofed-source replies are what re-seed the poisoned route.

// recordingConn records every Write so tests can assert which conn the hub routed to.
type recordingConn struct {
	net.Conn
	addr    string
	written chan []byte
	fail    atomic.Bool
}

func newRecordingConn(addr string) *recordingConn {
	return &recordingConn{addr: addr, written: make(chan []byte, 64)}
}

func (c *recordingConn) Write(b []byte) (int, error) {
	if c.fail.Load() {
		return 0, context.DeadlineExceeded
	}
	cp := append([]byte(nil), b...)
	select {
	case c.written <- cp:
	default:
	}
	return len(b), nil
}

func (c *recordingConn) RemoteAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", c.addr)
	return a
}

func (c *recordingConn) LocalAddr() net.Addr {
	a, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	return a
}

func (c *recordingConn) Close() error { return nil }

// --- Guard 1: route table never learns hub-owned IPs ---

func TestRouteTable_RefusesHubOwnedIP(t *testing.T) {
	hub := NewRouter()
	managerIP := net.ParseIP("10.101.198.81")
	hub.ReserveSelfIPs(managerIP)

	ctx := context.Background()
	spoofed := newRecordingConn("127.0.0.1:40001")

	// A packet whose source is the manager ClusterIP (a spoofed reply from a client
	// stack) must not register a route — but must keep the read loop alive (true).
	if ok := hub.routeTable.addRoute(ctx, managerIP.To4(), spoofed); !ok {
		t.Fatal("addRoute must return true for a reserved IP so the conn read loop survives")
	}
	if hub.HasRoute(string(managerIP.To4())) {
		t.Fatal("route table learned a route for the hub's own ClusterIP")
	}

	// Normal client routes are unaffected.
	clientIP := net.ParseIP("198.18.0.9")
	if ok := hub.routeTable.addRoute(ctx, clientIP.To4(), spoofed); !ok {
		t.Fatal("normal route registration should succeed")
	}
	if !hub.HasRoute(string(clientIP.To4())) {
		t.Fatal("expected the client route to be learned")
	}

	// IPv6 hub-owned IPs are normalized to the same 16-byte key form ParseIPFast yields.
	managerIP6 := net.ParseIP("2001:2::9002")
	hub.ReserveSelfIPs(managerIP6)
	if ok := hub.routeTable.addRoute(ctx, managerIP6.To16(), spoofed); !ok {
		t.Fatal("addRoute must return true for a reserved IPv6")
	}
	if hub.HasRoute(string(managerIP6.To16())) {
		t.Fatal("route table learned a route for the hub's own IPv6 address")
	}
}

// --- Guard 2: hub-owned destination bypasses a bogus route and goes to gvisor ---

func TestDispatch_HubOwnedDstHandledByStackNotRoute(t *testing.T) {
	hub := NewRouter()
	managerIP := net.ParseIP("10.101.198.81")
	clientIP := net.ParseIP("198.18.0.7")
	ctx := context.Background()

	hub.ReserveSelfIPs(managerIP)

	// A stale/bogus route for the ClusterIP, as the loop used to learn from a spoofed
	// source before the reservation existed (inserted directly to simulate pre-guard
	// state).
	bogus := newRecordingConn("127.0.0.1:40002")
	hub.mu.Lock()
	hub.routes[string(managerIP.To4())] = &ConnList{}
	hub.routes[string(managerIP.To4())].Add(bogus)
	hub.mu.Unlock()

	h := GvisorTCPHandler(hub).(*gvisorTCPHandler)
	hub.peers.Start(ctx)
	defer func() { _ = hub.peers.Close() }()

	// Client conn the packet "arrived on" — dispatch registers it as the route for the
	// client's TUN IP, so the stack's reply must land here.
	client := newRecordingConn("127.0.0.1:40003")

	icmpPkt, err := netutil.GenICMPPacket(clientIP, managerIP)
	if err != nil {
		t.Fatalf("build icmp packet: %v", err)
	}
	pkt := newPacketFromPool(config.LargeBufferSize)
	n := copy(pkt.data[tunReserve:], icmpPkt)
	pkt.length = n + typePrefixLen
	pkt.setType(packetTypeToGvisor)

	if !h.dispatchServerDataPacket(ctx, client, pkt, nil, nil, nil) {
		t.Fatal("dispatch must keep the read loop alive")
	}

	// The stack's inline ICMP forwarder answers (managerIP -> clientIP); the reply is
	// routed to the client's route conn. Without the guard the request would have been
	// bounced to the bogus conn instead and no reply would ever come back.
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case frame := <-client.written:
			if len(frame) < 4 {
				t.Fatalf("short frame written to client conn: %d bytes", len(frame))
			}
			if frame[2] != packetTypeToTUN {
				t.Fatalf("expected packetTypeToTUN reply, got prefix %d", frame[2])
			}
			src, dst, _, perr := netutil.ParseIPFast(frame[3:])
			if perr != nil {
				t.Fatalf("parse reply: %v", perr)
			}
			if !src.Equal(managerIP) || !dst.Equal(clientIP) {
				t.Fatalf("unexpected reply %s -> %s, want %s -> %s", src, dst, managerIP, clientIP)
			}
			return // success: handled by the local stack, not routed to the bogus conn
		default:
		}
		if len(bogus.written) > 0 {
			t.Fatal("hub-owned destination was routed to the bogus client route")
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the gvisor stack to answer the hub-owned destination")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- Guard 3: client does not inject foreign destinations into the inter-client stack ---

func newGuardSlot(t *testing.T, owned func(net.IP) bool) (*connSlot, *interClientStack, chan *packet, chan *packet, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	outbound := make(chan *packet, 8)
	stackOut := make(chan *packet, 8)
	ics := newInterClientStack(ctx, stackOut, datagramHeaderLen)
	slot := &connSlot{
		inbound:     make(chan *packet, 8),
		tunOutbound: outbound,
		interClient: ics,
		ownedDst:    owned,
	}
	return slot, ics, outbound, stackOut, cancel
}

func newToGvisorPacket(t *testing.T, ipPacket []byte) *packet {
	t.Helper()
	pkt := newPacketFromPool(config.LargeBufferSize)
	n := copy(pkt.data[tunReserve:], ipPacket)
	pkt.length = n + typePrefixLen
	pkt.setType(packetTypeToGvisor)
	pkt.src, pkt.dst, _, _ = netutil.ParseIPFast(pkt.ip())
	return pkt
}

func TestDispatchReadPacket_ForeignDstGoesToTunNotStack(t *testing.T) {
	clientIP := net.ParseIP("198.18.0.1")
	managerIP := net.ParseIP("10.101.198.81")

	slot, ics, outbound, stackOut, cancel := newGuardSlot(t, func(ip net.IP) bool { return ip.Equal(clientIP) })
	// cancel must run before waiting on ics.done, else the wait deadlocks (LIFO defers).
	defer func() { cancel(); <-ics.done }()

	a := &connAttempt{connSlot: slot}
	pkt := newToGvisorPacket(t, buildIPv4Packet(clientIP, managerIP, []byte("dns-query")))

	if !a.dispatchReadPacket(context.Background(), pkt, 0, nil, nil) {
		t.Fatal("dispatchReadPacket must keep the read loop alive")
	}

	// The foreign-destination packet must be written to the TUN (kernel routing), not
	// injected into the promiscuous stack whose replies would carry the ClusterIP as
	// source and re-seed the poisoned server route.
	select {
	case got := <-outbound:
		if !got.dst.Equal(managerIP) {
			t.Fatalf("unexpected dst %s on tunOutbound, want %s", got.dst, managerIP)
		}
		got.release()
	case <-time.After(time.Second):
		t.Fatal("foreign-destination packet was not written to tunOutbound")
	}
	select {
	case pkt := <-stackOut:
		t.Fatalf("foreign-destination packet was injected into the inter-client stack (stack emitted %s -> %s)", pkt.src, pkt.dst)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDispatchReadPacket_OwnedDstStillInjected(t *testing.T) {
	selfIP := net.ParseIP("198.18.0.1")
	peerIP := net.ParseIP("198.18.0.2")

	slot, ics, outbound, stackOut, cancel := newGuardSlot(t, func(ip net.IP) bool { return ip.Equal(selfIP) })
	// cancel must run before waiting on ics.done, else the wait deadlocks (LIFO defers).
	defer func() { cancel(); <-ics.done }()

	a := &connAttempt{connSlot: slot}

	// Inter-client ICMP echo addressed to this client's own TUN IP: must still be
	// injected (the stack's inline ICMP forwarder answers it).
	icmpPkt, err := netutil.GenICMPPacket(peerIP, selfIP)
	if err != nil {
		t.Fatalf("build icmp packet: %v", err)
	}
	pkt := newToGvisorPacket(t, icmpPkt)
	if !a.dispatchReadPacket(context.Background(), pkt, 0, nil, nil) {
		t.Fatal("dispatchReadPacket must keep the read loop alive")
	}

	select {
	case got := <-stackOut:
		src, dst, _, perr := netutil.ParseIPFast(got.ip())
		if perr != nil {
			t.Fatalf("parse stack output: %v", perr)
		}
		if !src.Equal(selfIP) || !dst.Equal(peerIP) {
			t.Fatalf("unexpected echo reply %s -> %s, want %s -> %s", src, dst, selfIP, peerIP)
		}
		got.release()
	case <-time.After(3 * time.Second):
		t.Fatal("owned-destination packet was not injected into the inter-client stack")
	}
	select {
	case pkt := <-outbound:
		t.Fatalf("owned-destination packet wrongly written to tunOutbound (%s -> %s)", pkt.src, pkt.dst)
	case <-time.After(300 * time.Millisecond):
	}
}

// Guard against a regression in the route-key normalization: the reserved set is keyed
// the same way route entries are (4-byte IPv4 / 16-byte IPv6 from ParseIPFast).
func TestRouteIPKey_MatchesParseIPFastKeys(t *testing.T) {
	managerIP := net.ParseIP("10.101.198.81")
	pkt := buildIPv4Packet(net.ParseIP("198.18.0.1"), managerIP, []byte("x"))
	_, dst, _, err := netutil.ParseIPFast(pkt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if routeIPKey(managerIP) != string(dst) {
		t.Fatalf("routeIPKey %q does not match packet dst key %q", routeIPKey(managerIP), string(dst))
	}
	if key := routeIPKey(managerIP); len(key) != 4 {
		t.Fatalf("expected 4-byte key for IPv4, got %d", len(key))
	}
}
