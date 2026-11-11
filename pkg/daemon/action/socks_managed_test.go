package action

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	xproxy "golang.org/x/net/proxy"
	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

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

// TestManagedSocksProxy_EgressLifecycle binds before start, performs a real
// SOCKS5 handshake and relay, then verifies connection cleanup releases the port.
func TestManagedSocksProxy_EgressLifecycle(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	connect := &handler.ConnectOptions{ConnectionID: "socks-test"}
	req := &rpc.ConnectRequest{EnableSocks: true, SocksEgress: true, SocksListen: "127.0.0.1:0"}
	proxy, err := prepareManagedSocksProxy(context.Background(), connect, req)
	if err != nil {
		t.Fatalf("prepare managed SOCKS proxy: %v", err)
	}
	proxy.start(connect)
	addr := connect.GetSocksListenAddr()
	if addr == "" || addr == "127.0.0.1:0" {
		t.Fatalf("SOCKS did not publish its bound address: %q", addr)
	}
	if !connect.GetSocksEgress() {
		t.Fatal("SocksEgress = false, want true")
	}
	if !waitFor(func() bool { return portAccepting(addr) }, 3*time.Second) {
		t.Fatalf("managed SOCKS proxy never started listening on %s", addr)
	}

	dialer, err := xproxy.SOCKS5("tcp", addr, nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return dialer.Dial("tcp", backend.Listener.Addr().String())
		},
	}}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("request through managed SOCKS proxy: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("SOCKS relay body = %q, err=%v", body, err)
	}

	connect.Cleanup(context.Background())
	if !waitFor(func() bool { return !portAccepting(addr) }, 3*time.Second) {
		t.Fatalf("managed SOCKS proxy still listening on %s after Cleanup", addr)
	}
}

func TestManagedSocksProxy_DisabledNoop(t *testing.T) {
	connect := &handler.ConnectOptions{ConnectionID: "no-socks"}
	proxy, err := prepareManagedSocksProxy(context.Background(), connect, &rpc.ConnectRequest{})
	if err != nil || proxy != nil {
		t.Fatalf("disabled SOCKS prepare = %v, %v", proxy, err)
	}
	proxy.start(connect)
	if connect.GetSocksListenAddr() != "" {
		t.Fatalf("expected no SocksListenAddr, got %q", connect.GetSocksListenAddr())
	}
}

type countingDaemonClient struct {
	rpc.DaemonClient
	connectCalls atomic.Int32
}

func (c *countingDaemonClient) Connect(context.Context, ...grpc.CallOption) (rpc.Daemon_ConnectClient, error) {
	c.connectCalls.Add(1)
	return nil, errors.New("root Connect must not be reached")
}

func TestManagedSocksProxy_OccupiedPortFailsBeforeRootConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connect := &handler.ConnectOptions{ConnectionID: "occupied-socks"}
	req := &rpc.ConnectRequest{EnableSocks: true, SocksEgress: true, SocksListen: listener.Addr().String()}
	rootClient := &countingDaemonClient{}
	svr := &Server{}

	err = svr.forwardConnectToSudo(context.Background(), req, connect, &fakeConnectServer{ctx: context.Background()}, rootClient, nil, nil, nil, "occupied-socks", logrus.New())
	if err == nil {
		t.Fatal("expected synchronous listen failure")
	}
	if rootClient.connectCalls.Load() != 0 {
		t.Fatalf("root Connect called %d times after local bind failure", rootClient.connectCalls.Load())
	}
	if connect.GetSocksListenAddr() != "" {
		t.Fatalf("failed SOCKS bind published address %q", connect.GetSocksListenAddr())
	}
}

func TestManagedSocksProxy_RestorePortConflictKeepsVPNRecovery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connect := &handler.ConnectOptions{ConnectionID: "restore-socks"}
	req := &rpc.ConnectRequest{EnableSocks: true, SocksEgress: true, SocksListen: listener.Addr().String()}

	proxy, err := prepareManagedSocksProxyForConnect(context.Background(), connect, req, true)
	if err != nil || proxy != nil {
		t.Fatalf("restore should continue without SOCKS: proxy=%v, err=%v", proxy, err)
	}
	if connect.GetSocksListenAddr() != "" {
		t.Fatalf("restore published unavailable SOCKS address %q", connect.GetSocksListenAddr())
	}
	if !req.GetEnableSocks() {
		t.Fatal("restore cleared the persisted SOCKS intent")
	}
}
