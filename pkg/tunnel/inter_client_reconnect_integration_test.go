package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

type trackedConnRole uint8

const (
	trackedConnUnknown trackedConnRole = iota
	trackedConnData
	trackedConnControl
)

// roleTrackedConn observes the first unencrypted frame below wire.Conn. Data
// connections announce with type=1; control connections declare with type=2.
type roleTrackedConn struct {
	net.Conn
	mu     sync.Mutex
	prefix []byte
	role   trackedConnRole
}

func (c *roleTrackedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.role == trackedConnUnknown {
		c.prefix = append(c.prefix, p...)
		if len(c.prefix) >= tunReserve {
			switch c.prefix[datagramHeaderLen] {
			case packetTypeToGvisor:
				c.role = trackedConnData
			case packetTypeControl:
				c.role = trackedConnControl
			}
		}
	}
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *roleTrackedConn) connectionRole() trackedConnRole {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.role
}

type roleTrackingTransport struct {
	mu    sync.Mutex
	conns []*roleTrackedConn
}

func (t *roleTrackingTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tracked := &roleTrackedConn{Conn: conn}
	t.mu.Lock()
	t.conns = append(t.conns, tracked)
	t.mu.Unlock()
	return tracked, nil
}

func (t *roleTrackingTransport) connectionCount(role trackedConnRole) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, conn := range t.conns {
		if conn.connectionRole() == role {
			count++
		}
	}
	return count
}

func (t *roleTrackingTransport) closeLatestData(tb testing.TB) {
	tb.Helper()
	t.mu.Lock()
	var latest *roleTrackedConn
	for _, conn := range t.conns {
		if conn.connectionRole() == trackedConnData {
			latest = conn
		}
	}
	t.mu.Unlock()
	if latest == nil {
		tb.Fatal("no classified data connection to close")
	}
	if err := latest.Close(); err != nil {
		tb.Fatalf("close tracked data connection: %v", err)
	}
}

func TestInterClient_DataReconnectDoesNotCrossRoutes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pinger        string
		pingee        string
		unrelatedPeer string
	}{
		{name: "ipv4", pinger: "198.18.0.5", pingee: "198.18.0.3", unrelatedPeer: "198.18.0.7"},
		{name: "ipv6", pinger: "2001:2::5", pingee: "2001:2::3", unrelatedPeer: "2001:2::7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			hub := newInterClientServer(ctx, t)
			tracking := &roleTrackingTransport{}
			clientA := startPipeClientWithOptions(ctx, t, hub.port, tc.pinger, pipeClientOptions{announce: true})
			_ = startPipeClientWithOptions(ctx, t, hub.port, tc.pingee, pipeClientOptions{transporter: tracking, announce: true})
			clientC := startPipeClientWithOptions(ctx, t, hub.port, tc.unrelatedPeer, pipeClientOptions{announce: true})

			waitForRoutes(t, hub.hub, tc.pinger, tc.pingee, tc.unrelatedPeer)
			waitForTrackedConnections(t, tracking, trackedConnData, 1)
			waitForTrackedConnections(t, tracking, trackedConnControl, 1)
			assertInterClientPing(t, clientA, tc.pinger, tc.pingee)
			assertNoCrossClientPacket(t, clientC, tc.pingee, tc.pinger)

			key := string(net.ParseIP(tc.pingee))
			if net.ParseIP(tc.pingee).To4() != nil {
				key = string(net.ParseIP(tc.pingee).To4())
			}
			members := routeSnapshot(hub.hub)[key]
			if len(members) == 0 {
				t.Fatal("pingee route disappeared before reconnect")
			}
			previous := members[0]
			tracking.closeLatestData(t)

			waitForTrackedConnections(t, tracking, trackedConnData, 2)
			deadline := time.Now().Add(10 * time.Second)
			for {
				members = routeSnapshot(hub.hub)[key]
				if len(members) > 0 && members[0] != previous {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("replacement data connection did not become the newest route member")
				}
				time.Sleep(20 * time.Millisecond)
			}

			assertInterClientPing(t, clientA, tc.pinger, tc.pingee)
			assertNoCrossClientPacket(t, clientC, tc.pingee, tc.pinger)
			if got := tracking.connectionCount(trackedConnControl); got != 1 {
				t.Fatalf("data reconnect replaced control connection: count=%d", got)
			}
		})
	}
}

func waitForTrackedConnections(t testing.TB, transport *roleTrackingTransport, role trackedConnRole, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for transport.connectionCount(role) < want {
		if time.Now().After(deadline) {
			t.Fatalf("tracked %v connections did not reach %d", role, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertInterClientPing(t *testing.T, pinger pipeClient, src, dst string) {
	t.Helper()
	if _, err := pinger.os.Write(genEchoRequest(t, src, dst)); err != nil {
		t.Fatalf("write ping after reconnect: %v", err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case packet := <-pinger.replies:
			from, to, _, err := netutil.ParseIPFast(packet)
			if err != nil || !from.Equal(net.ParseIP(dst)) || !to.Equal(net.ParseIP(src)) {
				continue
			}
			if err := verifyEchoReplyChecksum(packet); err != nil {
				t.Fatalf("invalid reconnected echo reply: %v", err)
			}
			return
		case <-timer.C:
			t.Fatalf("no valid echo reply %s -> %s", dst, src)
		}
	}
}

func assertNoCrossClientPacket(t testing.TB, unrelated pipeClient, src, dst string) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case packet := <-unrelated.packets:
			from, to, _, err := netutil.ParseIPFast(packet)
			if err == nil && from.Equal(net.ParseIP(src)) && to.Equal(net.ParseIP(dst)) {
				t.Fatalf("unrelated client received cross-routed packet %s -> %s", from, to)
			}
		case <-timer.C:
			return
		}
	}
}

func (r trackedConnRole) String() string {
	switch r {
	case trackedConnData:
		return "data"
	case trackedConnControl:
		return "control"
	default:
		return fmt.Sprintf("unknown(%d)", r)
	}
}
