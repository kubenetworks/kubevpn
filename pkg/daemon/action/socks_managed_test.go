package action

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// freeLoopbackAddr returns a currently-free 127.0.0.1 address by binding port 0 and
// immediately releasing it. Good enough for a test (tiny reuse race).
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func portAccepting(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestManagedSocksProxy_EgressLifecycle wires startManagedSocksProxy to a real
// localproxy.Server in host-egress mode (no cluster client needed) and verifies the
// full daemon-managed lifecycle: the proxy starts listening, its state is recorded on
// the connection, and Cleanup (which runs the registered rollback) stops it and frees
// the port. This is the daemon-side counterpart to the CLI's former subprocess model.
func TestManagedSocksProxy_EgressLifecycle(t *testing.T) {
	addr := freeLoopbackAddr(t)
	connect := &handler.ConnectOptions{ConnectionID: "socks-test"}
	req := &rpc.ConnectRequest{
		EnableSocks: true,
		SocksEgress: true,
		SocksListen: addr,
	}

	startManagedSocksProxy(context.Background(), connect, req)

	if connect.GetSocksListenAddr() != addr {
		t.Fatalf("SocksListenAddr = %q, want %q", connect.GetSocksListenAddr(), addr)
	}
	if !connect.GetSocksEgress() {
		t.Fatal("SocksEgress = false, want true")
	}
	if !waitFor(func() bool { return portAccepting(addr) }, 3*time.Second) {
		t.Fatalf("managed SOCKS proxy never started listening on %s", addr)
	}

	// Cleanup runs the connection's rollback funcs, which cancels the proxy context.
	connect.Cleanup(context.Background())

	if !waitFor(func() bool { return !portAccepting(addr) }, 3*time.Second) {
		t.Fatalf("managed SOCKS proxy still listening on %s after Cleanup", addr)
	}
}

// TestManagedSocksProxy_DisabledNoop verifies that without EnableSocks nothing is started
// and no proxy state is recorded.
func TestManagedSocksProxy_DisabledNoop(t *testing.T) {
	connect := &handler.ConnectOptions{ConnectionID: "no-socks"}
	startManagedSocksProxy(context.Background(), connect, &rpc.ConnectRequest{})
	if connect.GetSocksListenAddr() != "" {
		t.Fatalf("expected no SocksListenAddr, got %q", connect.GetSocksListenAddr())
	}
}
