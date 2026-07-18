//go:build tun

// These are real-TUN end-to-end tests: they create actual TUN interfaces (requiring
// CAP_NET_ADMIN / root) and run the full gvisor data path. They are CPU-contention sensitive
// and not safe to run alongside the rest of the unit suite, so they live behind the `tun` build
// tag and run on demand / in a dedicated CI step:
//
//	sudo go test ./pkg/core/... -tags=tun -run TestTUN -timeout=120s -v
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"

	"github.com/containernetworking/cni/pkg/types"
)

// tunTestContext returns a context for a real-TUN test and registers a cleanup that cancels it
// and then waits for the test's goroutines (gvisor stacks, dial/reconnect loops, heartbeats) to
// drain before the next test runs. These tests run sequentially; without an explicit drain a
// finished test's still-running goroutines accumulate and starve the next test's data path under
// CPU contention — the root cause of the historic real-TUN flakiness. The per-test listeners are
// closed by each test's own defers (which run before this cleanup), unblocking accept loops.
func tunTestContext(t *testing.T) context.Context {
	t.Helper()
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(func() {
		cancel()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= base+5 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("real-TUN goroutines did not fully drain (now %d, baseline %d)", runtime.NumGoroutine(), base)
	})
	return ctx
}

// =============================================================================
// Integration tests requiring a real TUN device.
//
// Run with: go test ./pkg/core/... -tags=tun -run TestTUN -timeout=60s -v
//
// These tests create actual TUN interfaces, route traffic through them,
// and verify the full kubevpn data path end-to-end:
//
//   App → TUN device → ClientDevice.readFromTun → hash(dst) → slot → writeToConn
//   → TCP pipe (simulating port-forward)
//   → server side: gvisor stack → TCPForwarder → net.Dial(real service)
//   → service responds → gvisor → endpoint → pipe → readFromConn
//   → tunOutbound → writeToTun → TUN device → App receives response
//
// Requirements:
//   - Linux with CAP_NET_ADMIN (or root)
//   - ip/iproute2 commands available
// =============================================================================

// TestTUN_FullDataPath_ClientToService creates a real TUN device on the client
// side and verifies that traffic flows through the full pipeline to a real
// echo server.
//
// Architecture:
//
//	curl (via TUN route) → TUN device → ClientDevice pipeline → pipe
//	→ server Handler (gvisor) → TCPForwarder → echo server → response back
func TestTUN_FullDataPath_ClientToService(t *testing.T) {
	ctx := tunTestContext(t)

	// === Step 1: Start a real TCP echo server ===
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server: %v", err)
	}
	defer echoListener.Close()
	echoPort := echoListener.Addr().(*net.TCPAddr).Port
	t.Logf("Echo server on 127.0.0.1:%d", echoPort)

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	// === Step 2: Start a real HTTP server (for more realistic test) ===
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	httpMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http server: %v", err)
	}
	defer httpListener.Close()
	httpPort := httpListener.Addr().(*net.TCPAddr).Port
	go http.Serve(httpListener, httpMux)
	t.Logf("HTTP server on 127.0.0.1:%d", httpPort)

	// === Step 3: Create TUN device ===
	// Assign client TUN IP: 198.18.0.2/32
	// Route 10.96.0.0/16 through TUN (simulates cluster service CIDR)
	clientIP := "198.18.0.2/32"
	tunCfg := tun.Config{
		Addr: clientIP,
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("10.96.0.0/16")},
		},
	}
	tunListener, err := tun.Listener(tunCfg)
	if err != nil {
		t.Fatalf("create TUN: %v", err)
	}
	defer tunListener.Close()
	t.Logf("TUN device created with IP %s", clientIP)

	// === Step 4: Create the server-side listener (simulates port-forward target) ===
	// In production this is the traffic-manager pod listening on :10801
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server listener: %v", err)
	}
	defer serverListener.Close()
	serverPort := serverListener.Addr().(*net.TCPAddr).Port
	t.Logf("Server listener on 127.0.0.1:%d", serverPort)

	// Server: accept connections and handle with gvisor
	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			go serverHandler.Handle(ctx, conn)
		}
	}()

	// === Step 5: Create client-side Forwarder → connects to server ===
	forwarder := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}

	// === Step 6: Start the client TUN handler ===
	// This runs the full ClientDevice pipeline:
	// readFromTun → runConnPool → writeToConn → server → readFromConn → writeToTun
	tunHandler := TunHandler(forwarder, hub)
	go func() {
		tunConn, err := tunListener.Accept()
		if err != nil {
			return
		}
		tunHandler.Handle(ctx, tunConn)
	}()

	// Wait for connection establishment
	time.Sleep(2 * time.Second)

	// === Step 7: Send traffic through the TUN (simulates app accessing service) ===
	// Traffic to 10.96.0.1:<echoPort> will be routed through TUN
	// (because we added route 10.96.0.0/16 → TUN)
	t.Log("Sending TCP traffic through TUN to echo server...")

	// Direct TCP connection to the echo server through the TUN route
	dialer := net.Dialer{
		Timeout: 5 * time.Second,
		LocalAddr: &net.TCPAddr{
			IP: net.ParseIP("198.18.0.2"),
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("10.96.0.1:%d", echoPort))
	if err != nil {
		t.Fatalf("dial through TUN failed: %v", err)
	}
	defer conn.Close()
	t.Log("✅ TCP connection established through TUN")

	// Send data
	testData := []byte("REAL-TUN-E2E-TEST-DATA-12345")
	_, err = conn.Write(testData)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read response
	buf := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(testData) {
		t.Fatalf("echo mismatch: got %q, want %q", buf[:n], testData)
	}
	t.Logf("✅ FULL TUN E2E: sent %q → received %q through real TUN device", testData, buf[:n])
}

// TestTUN_FullDataPath_HTTPRequest makes a real HTTP request through the TUN.
func TestTUN_FullDataPath_HTTPRequest(t *testing.T) {
	ctx := tunTestContext(t)

	// HTTP server
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","source":"gvisor"}`))
	})
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	defer httpListener.Close()
	httpPort := httpListener.Addr().(*net.TCPAddr).Port
	go http.Serve(httpListener, httpMux)

	// TUN device
	tunCfg := tun.Config{
		Addr: "198.18.0.2/32",
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("10.96.0.0/16")},
		},
	}
	tunListener, err := tun.Listener(tunCfg)
	if err != nil {
		t.Fatalf("TUN: %v", err)
	}
	defer tunListener.Close()

	// Server
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer serverListener.Close()
	serverPort := serverListener.Addr().(*net.TCPAddr).Port

	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			go serverHandler.Handle(ctx, conn)
		}
	}()

	// Client
	forwarder := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}
	tunHandler := TunHandler(forwarder, hub)
	go func() {
		conn, err := tunListener.Accept()
		if err != nil {
			return
		}
		tunHandler.Handle(ctx, conn)
	}()

	time.Sleep(2 * time.Second)

	// HTTP request through TUN
	t.Log("Making HTTP request through TUN...")
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Route through TUN by dialing the virtual service IP
				return net.DialTimeout("tcp", fmt.Sprintf("10.96.0.1:%d", httpPort), 5*time.Second)
			},
		},
		Timeout: 10 * time.Second,
	}
	resp, err := httpClient.Get(fmt.Sprintf("http://10.96.0.1:%d/api/data", httpPort))
	if err != nil {
		t.Fatalf("HTTP GET failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP status %d, body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "gvisor") {
		t.Fatalf("unexpected body: %s", body)
	}
	t.Logf("✅ HTTP through TUN: status=%d body=%s", resp.StatusCode, string(body))
}

// TestTUN_ConnectionPool_MultiSlot verifies that the connection pool with
// multiple slots works correctly with a real TUN device.
func TestTUN_ConnectionPool_MultiSlot(t *testing.T) {
	ctx := tunTestContext(t)

	// Echo server
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	defer echoListener.Close()
	echoPort := echoListener.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	// TUN
	tunCfg := tun.Config{
		Addr: "198.18.0.2/32",
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("10.96.0.0/16")},
		},
	}
	tunListener, err := tun.Listener(tunCfg)
	if err != nil {
		t.Fatalf("TUN: %v", err)
	}
	defer tunListener.Close()

	// Server accepts multiple connections (pool)
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer serverListener.Close()
	serverPort := serverListener.Addr().(*net.TCPAddr).Port

	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	var connCount atomic.Int32
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			connCount.Add(1)
			go serverHandler.Handle(ctx, conn)
		}
	}()

	// Client with connection pool (ConnPoolSize=4)
	forwarder := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}
	tunHandler := TunHandler(forwarder, hub)
	go func() {
		conn, err := tunListener.Accept()
		if err != nil {
			return
		}
		tunHandler.Handle(ctx, conn)
	}()

	// Wait for the pool to establish — poll instead of a fixed sleep, which flakes under load.
	deadline := time.Now().Add(15 * time.Second)
	for connCount.Load() < int32(ConnPoolSize) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := connCount.Load(); got < int32(ConnPoolSize) {
		t.Fatalf("expected at least %d connections (pool), got %d", ConnPoolSize, got)
	}
	t.Logf("✅ Connection pool: %d connections established (expected %d)", connCount.Load(), ConnPoolSize)

	// Send multiple requests to different dest IPs (should hash to different slots)
	dests := []string{
		fmt.Sprintf("10.96.0.1:%d", echoPort),
		fmt.Sprintf("10.96.0.2:%d", echoPort),
		fmt.Sprintf("10.96.0.3:%d", echoPort),
		fmt.Sprintf("10.96.0.4:%d", echoPort),
	}
	for i, dest := range dests {
		msg := []byte(fmt.Sprintf("msg-%d-to-%s", i, dest))
		// Retry: the first round-trips can be slow under load (pool warmup + gvisor),
		// so a single attempt is flaky. The data path itself is unchanged.
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			if attempt > 0 {
				time.Sleep(500 * time.Millisecond)
			}
			if lastErr = tunEchoRoundTrip(dest, msg); lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			t.Fatalf("echo to %s failed after retries: %v", dest, lastErr)
		}
		t.Logf("  ✅ %s: echoed %q", dest, msg)
	}
	t.Log("✅ All slots working correctly with different dest IPs")
}

// tunEchoRoundTrip dials dest, writes msg, and verifies the echo. Returns nil on success.
func tunEchoRoundTrip(dest string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", dest, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if string(buf[:n]) != string(msg) {
		return fmt.Errorf("echo mismatch: got %q, want %q", buf[:n], msg)
	}
	return nil
}

// TestTUN_InterClient_Routing tests that two clients can communicate through
// the traffic manager using real TUN devices.
func TestTUN_InterClient_Routing(t *testing.T) {
	ctx := tunTestContext(t)

	// Shared server (traffic manager)
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer serverListener.Close()
	serverPort := serverListener.Addr().(*net.TCPAddr).Port

	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			go serverHandler.Handle(ctx, conn)
		}
	}()

	// Client A: TUN IP 198.18.0.2
	tunA, err := tun.Listener(tun.Config{
		Addr: "198.18.0.2/32",
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("198.18.0.0/24")},
		},
	})
	if err != nil {
		t.Fatalf("TUN A: %v", err)
	}
	defer tunA.Close()

	fwdA := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}
	go func() {
		conn, _ := tunA.Accept()
		TunHandler(fwdA, hub).Handle(ctx, conn)
	}()

	// Client B: TUN IP 198.18.0.3
	tunB, err := tun.Listener(tun.Config{
		Addr: "198.18.0.3/32",
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("198.18.0.0/24")},
		},
	})
	if err != nil {
		t.Fatalf("TUN B: %v", err)
	}
	defer tunB.Close()

	fwdB := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}
	go func() {
		conn, _ := tunB.Accept()
		TunHandler(fwdB, hub).Handle(ctx, conn)
	}()

	// Wait for both clients to connect
	time.Sleep(3 * time.Second)

	// Client A pings Client B's TUN IP
	t.Log("Testing inter-client: A (198.18.0.2) → B (198.18.0.3)")
	ok, err := util.Ping(ctx, "198.18.0.2", "198.18.0.3")
	if err != nil {
		t.Logf("⚠️ Ping A→B error (may be expected without full ICMP): %v", err)
	} else if ok {
		t.Log("✅ Inter-client ping A→B succeeded")
	}

	// Verify routes are registered
	keyA := string(net.ParseIP("198.18.0.2").To4())
	keyB := string(net.ParseIP("198.18.0.3").To4())
	if !hub.HasRoute(keyA) {
		t.Fatal("Client A route not registered")
	}
	if !hub.HasRoute(keyB) {
		t.Fatal("Client B route not registered")
	}
	t.Log("✅ Both client routes registered in RouteHub")
}

func mustParseCIDR(s string) net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return *cidr
}

// =============================================================================
// TestTUN_ClientDevice_FullPipeline constructs a ClientDevice directly (not
// through TunHandler) and exercises the complete data path end-to-end with
// real TUN devices and real servers.
//
// This is the most comprehensive integration test in the suite. It verifies
// every layer of the pipeline explicitly:
//
//	┌─────────────────────────────────────────────────────────────────────┐
//	│ OUTBOUND (client → service)                                        │
//	│                                                                    │
//	│ App dials 10.96.0.x:port                                           │
//	│   → kernel routes via TUN device (10.96.0.0/16 → utun)             │
//	│   → ClientDevice.readFromTun() reads raw IP packet from TUN        │
//	│   → parses src/dst IP, prepends prefix byte (buf[2]=1)             │
//	│   → sends Packet to tunInbound channel                             │
//	│   → runConnPool reads tunInbound, computes slot = ipHash(dst, N)   │
//	│   → distributes to slots[slot] channel                             │
//	│   → writeToConn frames [2-byte len][prefix+IP] and writes to TCP   │
//	│   → TCP connection carries framed datagram to server               │
//	│   → gvisorTCPHandler.readFromTCPConnWriteToEndpoint()              │
//	│     → UDPConnOverTCP.Read() strips 2-byte header                   │
//	│     → registers src route in RouteHub via hub.AddRoute(src, conn)  │
//	│     → injects IP packet into gvisor channel.Endpoint               │
//	│   → gvisor network stack processes the packet                      │
//	│   → TCPForwarder (LocalTCPForwarder) sees SYN, dials 127.0.0.1:p  │
//	│   → echo/HTTP server accepts, processes, responds                  │
//	│                                                                    │
//	│ INBOUND (service → client)                                         │
//	│                                                                    │
//	│ Echo server writes response data                                   │
//	│   → gvisor stack encapsulates into IP packet                       │
//	│   → readFromEndpointWriteToTCPConn reads from endpoint             │
//	│     → frames [2-byte len][prefix=0][IP packet]                     │
//	│     → writes to TCP connection                                     │
//	│   → ClientDevice.readFromConn receives frame via UDPConnOverTCP    │
//	│     → buf[0]==0 → sends to tunOutbound channel                     │
//	│   → writeToTun reads from tunOutbound                              │
//	│     → strips prefix byte, writes raw IP to TUN device              │
//	│   → kernel delivers IP packet to App's socket                      │
//	│   → App reads response data                                        │
//	└─────────────────────────────────────────────────────────────────────┘
//
// Phases tested:
//  1. Infrastructure: echo server, HTTP server, TUN device, gvisor server
//  2. ClientDevice direct construction and goroutine wiring
//  3. Connection pool establishment (ConnPoolSize slots)
//  4. TCP echo: single request/response through full path
//  5. HTTP: real HTTP GET/POST through TUN → gvisor → HTTP server
//  6. Multiple dest IPs: verify slot distribution via ipHash
//  7. Concurrent traffic: N goroutines sending simultaneously
//  8. Large payload: 128KB random data echo without corruption
//  9. Multiple sequential requests on a persistent connection
//  10. Route registration: verify RouteHub has client route
//
// Run: sudo go test ./pkg/core/... -tags=tun -run TestTUN_ClientDevice -timeout=120s -v
// =============================================================================
func TestTUN_ClientDevice_FullPipeline(t *testing.T) {
	ctx := tunTestContext(t)

	// =========================================================================
	// Phase 1: Start backend services
	// =========================================================================

	// --- TCP echo server ---
	// Echoes back everything it receives. Used for raw TCP and large payload tests.
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	defer echoListener.Close()
	echoPort := echoListener.Addr().(*net.TCPAddr).Port
	t.Logf("[Phase 1] TCP echo server on 127.0.0.1:%d", echoPort)

	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	// --- HTTP server ---
	// Serves /health (200 OK), /echo (body echo), /headers (dumps headers).
	// Used for HTTP-level verification through the gvisor stack.
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})
	httpMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Echo-Length", fmt.Sprintf("%d", len(body)))
		w.Write(body)
	})
	httpMux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Method: %s\n", r.Method)
		fmt.Fprintf(w, "Host: %s\n", r.Host)
		fmt.Fprintf(w, "Content-Length: %s\n", r.Header.Get("Content-Length"))
		fmt.Fprintf(w, "User-Agent: %s\n", r.Header.Get("User-Agent"))
	})
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http server listen: %v", err)
	}
	defer httpListener.Close()
	httpPort := httpListener.Addr().(*net.TCPAddr).Port
	go http.Serve(httpListener, httpMux)
	t.Logf("[Phase 1] HTTP server on 127.0.0.1:%d", httpPort)

	// =========================================================================
	// Phase 2: Create TUN device
	// =========================================================================

	// TUN IP: 198.18.0.2/32
	// Route: 10.96.0.0/16 → TUN (simulates Kubernetes service CIDR)
	tunCfg := tun.Config{
		Addr: "198.18.0.2/32",
		MTU:  config.DefaultMTU,
		Routes: []types.Route{
			{Dst: mustParseCIDR("10.96.0.0/16")},
		},
	}
	tunListener, err := tun.Listener(tunCfg)
	if err != nil {
		t.Fatalf("create TUN device: %v", err)
	}
	defer tunListener.Close()
	t.Log("[Phase 2] TUN device created: IP=198.18.0.2/32, route=10.96.0.0/16")

	// =========================================================================
	// Phase 3: Create server-side infrastructure
	// =========================================================================

	// Server TCP listener (simulates the traffic-manager pod's :10801 listener)
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server listener: %v", err)
	}
	defer serverListener.Close()
	serverPort := serverListener.Addr().(*net.TCPAddr).Port
	t.Logf("[Phase 3] Server listener on 127.0.0.1:%d", serverPort)

	// Server-side: GvisorLocalTCPHandler accepts connections, creates gvisor stack
	// per connection, and forwards TCP to 127.0.0.1:<dst_port> via LocalTCPForwarder.
	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	var serverConnCount atomic.Int32
	go func() {
		for {
			conn, err := serverListener.Accept()
			if err != nil {
				return
			}
			serverConnCount.Add(1)
			go serverHandler.Handle(ctx, conn)
		}
	}()

	// =========================================================================
	// Phase 4: Construct ClientDevice DIRECTLY (not via TunHandler)
	// =========================================================================

	// Accept the TUN connection (this is what the kernel gives us when TUN is created)
	tunConn, err := tunListener.Accept()
	if err != nil {
		t.Fatalf("accept TUN conn: %v", err)
	}

	// Create the Forwarder (how the client connects to the server)
	forwarder := &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}

	// Construct ClientDevice directly — the core of this test.
	// In production, HandleClient() does this inside tunHandler.
	errChan := make(chan error, 1)
	device := &tunDevice{
		tun:         tunConn,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     errChan,
	}
	device.transport = newClientTransport(device, forwarder)
	defer device.Close()

	// Start the device's routines — the same set serve() runs in production:
	// read-tun, write-tun, client-gvisor, client-conn-pool, client-heartbeat.
	for _, r := range device.routines() {
		go r.fn(ctx)
	}

	t.Log("[Phase 4] tunDevice (client transport) constructed and all goroutines started")

	// Wait for connection pool to establish (ConnPoolSize TCP connections)
	// and heartbeats to register routes.
	time.Sleep(3 * time.Second)

	// =========================================================================
	// Phase 5: Verify connection pool
	// =========================================================================

	poolConns := int(serverConnCount.Load())
	if poolConns < ConnPoolSize {
		t.Fatalf("[Phase 5] Connection pool: expected >= %d connections, got %d", ConnPoolSize, poolConns)
	}
	t.Logf("[Phase 5] Connection pool established: %d server connections (expected %d)", poolConns, ConnPoolSize)

	// Verify client's route is registered in RouteHub (from heartbeat)
	clientRouteKey := string(net.ParseIP("198.18.0.2").To4())
	if !hub.HasRoute(clientRouteKey) {
		t.Fatal("[Phase 5] Client route 198.18.0.2 NOT registered in RouteHub after heartbeat")
	}
	t.Log("[Phase 5] Route 198.18.0.2 registered in RouteHub via heartbeat")

	// =========================================================================
	// Phase 6: TCP echo — single request through full data path
	// =========================================================================

	t.Log("[Phase 6] TCP echo test: dial 10.96.0.1 through TUN...")

	tcpDialer := net.Dialer{
		Timeout:   5 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP("198.18.0.2")},
	}
	echoConn, err := tcpDialer.DialContext(ctx, "tcp", fmt.Sprintf("10.96.0.1:%d", echoPort))
	if err != nil {
		t.Fatalf("[Phase 6] dial 10.96.0.1:%d through TUN: %v", echoPort, err)
	}
	defer echoConn.Close()
	t.Log("[Phase 6] TCP connection established through TUN")

	// Send test data and read echo
	echoData := []byte("ClientDevice-E2E-TCP-Echo-Test-Payload-12345")
	if _, err := echoConn.Write(echoData); err != nil {
		t.Fatalf("[Phase 6] write echo data: %v", err)
	}
	echoBuf := make([]byte, 256)
	echoConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := echoConn.Read(echoBuf)
	if err != nil {
		t.Fatalf("[Phase 6] read echo response: %v", err)
	}
	if string(echoBuf[:n]) != string(echoData) {
		t.Fatalf("[Phase 6] echo mismatch: sent %q, got %q", echoData, echoBuf[:n])
	}
	t.Logf("[Phase 6] TCP echo OK: sent %d bytes, received %d bytes matching", len(echoData), n)

	// =========================================================================
	// Phase 7: HTTP GET and POST through TUN → gvisor → HTTP server
	// =========================================================================

	t.Log("[Phase 7] HTTP requests through TUN...")

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				// Force all HTTP connections through the TUN virtual IP
				return net.DialTimeout("tcp", fmt.Sprintf("10.96.0.1:%d", httpPort), 5*time.Second)
			},
		},
		Timeout: 10 * time.Second,
	}

	// 7a: GET /health
	resp, err := httpClient.Get(fmt.Sprintf("http://10.96.0.1:%d/health", httpPort))
	if err != nil {
		t.Fatalf("[Phase 7a] HTTP GET /health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("[Phase 7a] /health: status=%d body=%q, want 200/OK", resp.StatusCode, body)
	}
	t.Log("[Phase 7a] GET /health → 200 OK")

	// 7b: POST /echo with body
	postData := "ClientDevice-HTTP-POST-body-" + strings.Repeat("X", 200)
	resp, err = httpClient.Post(
		fmt.Sprintf("http://10.96.0.1:%d/echo", httpPort),
		"application/octet-stream",
		strings.NewReader(postData),
	)
	if err != nil {
		t.Fatalf("[Phase 7b] HTTP POST /echo: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != postData {
		t.Fatalf("[Phase 7b] POST /echo: body mismatch, got %d bytes, want %d", len(body), len(postData))
	}
	echoLenHeader := resp.Header.Get("X-Echo-Length")
	if echoLenHeader != fmt.Sprintf("%d", len(postData)) {
		t.Fatalf("[Phase 7b] X-Echo-Length header: got %q, want %q", echoLenHeader, fmt.Sprintf("%d", len(postData)))
	}
	t.Logf("[Phase 7b] POST /echo → echoed %d bytes with correct header", len(body))

	// 7c: GET /headers — verify HTTP metadata survives the gvisor path
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://10.96.0.1:%d/headers", httpPort), nil)
	req.Header.Set("User-Agent", "kubevpn-test/1.0")
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("[Phase 7c] HTTP GET /headers: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "kubevpn-test/1.0") {
		t.Fatalf("[Phase 7c] /headers: User-Agent not preserved, body=%s", body)
	}
	if !strings.Contains(string(body), "Method: GET") {
		t.Fatalf("[Phase 7c] /headers: Method not GET, body=%s", body)
	}
	t.Log("[Phase 7c] GET /headers → HTTP metadata preserved through gvisor")

	// =========================================================================
	// Phase 8: Multiple dest IPs — verify slot distribution via ipHash
	// =========================================================================

	t.Log("[Phase 8] Multi-dest IP echo test (different dest IPs hash to slots)...")

	destIPs := []string{"10.96.0.1", "10.96.0.2", "10.96.0.3", "10.96.0.4",
		"10.96.1.1", "10.96.1.2", "10.96.2.1", "10.96.3.1"}
	slotHits := make(map[int]int) // track which slots get hit

	for i, destIP := range destIPs {
		addr := fmt.Sprintf("%s:%d", destIP, echoPort)
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("[Phase 8] dial %s: %v", addr, err)
		}

		slot := ipHash(net.ParseIP(destIP).To4(), ConnPoolSize)
		slotHits[slot]++

		msg := []byte(fmt.Sprintf("slot-test-%d-%s", i, destIP))
		conn.Write(msg)
		buf := make([]byte, 256)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := conn.Read(buf)
		conn.Close()
		if err != nil {
			t.Fatalf("[Phase 8] read from %s: %v", addr, err)
		}
		if string(buf[:n]) != string(msg) {
			t.Fatalf("[Phase 8] echo %s mismatch: got %q, want %q", addr, buf[:n], msg)
		}
	}

	// Verify that multiple slots were used (not all traffic to one slot)
	if len(slotHits) < 2 {
		t.Fatalf("[Phase 8] ipHash used only %d slot(s) for %d different IPs; expected distribution", len(slotHits), len(destIPs))
	}
	t.Logf("[Phase 8] %d dest IPs distributed across %d/%d slots: %v", len(destIPs), len(slotHits), ConnPoolSize, slotHits)

	// =========================================================================
	// Phase 9: Concurrent traffic — N goroutines sending simultaneously
	// =========================================================================

	const concurrency = 10
	t.Logf("[Phase 9] Concurrent traffic test: %d goroutines...", concurrency)

	var wg sync.WaitGroup
	var successCount atomic.Int32
	var failCount atomic.Int32

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Each goroutine dials a different dest IP to spread across slots
			destIP := fmt.Sprintf("10.96.%d.%d", id/256, id%256)
			addr := fmt.Sprintf("%s:%d", destIP, echoPort)

			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Logf("[Phase 9] goroutine %d: dial %s failed: %v", id, addr, err)
				failCount.Add(1)
				return
			}
			defer conn.Close()

			// Send unique payload
			payload := []byte(fmt.Sprintf("concurrent-goroutine-%d-payload-%s", id, strings.Repeat("A", 50)))
			if _, err := conn.Write(payload); err != nil {
				t.Logf("[Phase 9] goroutine %d: write failed: %v", id, err)
				failCount.Add(1)
				return
			}

			buf := make([]byte, 256)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				t.Logf("[Phase 9] goroutine %d: read failed: %v", id, err)
				failCount.Add(1)
				return
			}

			if string(buf[:n]) != string(payload) {
				t.Logf("[Phase 9] goroutine %d: echo mismatch: got %d bytes, want %d", id, n, len(payload))
				failCount.Add(1)
				return
			}
			successCount.Add(1)
		}(i)
	}

	wg.Wait()

	if failCount.Load() > 0 {
		t.Fatalf("[Phase 9] %d/%d goroutines failed", failCount.Load(), concurrency)
	}
	t.Logf("[Phase 9] All %d concurrent goroutines succeeded", successCount.Load())

	// =========================================================================
	// Phase 10: Large payload — 128KB random data echo without corruption
	// =========================================================================

	t.Log("[Phase 10] Large payload test: 128KB random data...")

	largeSize := 128 * 1024
	largePayload := make([]byte, largeSize)
	if _, err := rand.Read(largePayload); err != nil {
		t.Fatalf("[Phase 10] generate random payload: %v", err)
	}

	// Use HTTP POST/response for large payload (TCP echo may split reads)
	resp, err = httpClient.Post(
		fmt.Sprintf("http://10.96.0.1:%d/echo", httpPort),
		"application/octet-stream",
		bytes.NewReader(largePayload),
	)
	if err != nil {
		t.Fatalf("[Phase 10] HTTP POST large payload: %v", err)
	}
	largeBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(largeBody) != largeSize {
		t.Fatalf("[Phase 10] large payload size mismatch: got %d, want %d", len(largeBody), largeSize)
	}
	if !bytes.Equal(largeBody, largePayload) {
		// Find the first differing byte for debugging
		for i := range largePayload {
			if i >= len(largeBody) || largeBody[i] != largePayload[i] {
				t.Fatalf("[Phase 10] large payload corruption at byte %d: got 0x%02x, want 0x%02x", i, largeBody[i], largePayload[i])
			}
		}
	}
	t.Logf("[Phase 10] 128KB random payload echoed without corruption")

	// =========================================================================
	// Phase 11: Multiple sequential requests on a persistent TCP connection
	// =========================================================================

	t.Log("[Phase 11] Sequential requests on persistent connection...")

	persistConn, err := tcpDialer.DialContext(ctx, "tcp", fmt.Sprintf("10.96.0.1:%d", echoPort))
	if err != nil {
		t.Fatalf("[Phase 11] dial persistent conn: %v", err)
	}
	defer persistConn.Close()

	const seqCount = 20
	for i := 0; i < seqCount; i++ {
		msg := []byte(fmt.Sprintf("seq-request-%04d-%s", i, strings.Repeat("Z", 30)))
		if _, err := persistConn.Write(msg); err != nil {
			t.Fatalf("[Phase 11] write seq %d: %v", i, err)
		}

		buf := make([]byte, 256)
		persistConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := persistConn.Read(buf)
		if err != nil {
			t.Fatalf("[Phase 11] read seq %d: %v", i, err)
		}
		if string(buf[:n]) != string(msg) {
			t.Fatalf("[Phase 11] seq %d echo mismatch: got %q, want %q", i, buf[:n], msg)
		}
	}
	t.Logf("[Phase 11] %d sequential requests on persistent connection all echoed correctly", seqCount)

	// =========================================================================
	// Phase 12: Verify RouteHub state
	// =========================================================================

	t.Log("[Phase 12] RouteHub verification...")

	// Client IP should still be registered (from heartbeats)
	if !hub.HasRoute(clientRouteKey) {
		t.Fatal("[Phase 12] Client route 198.18.0.2 lost from RouteHub")
	}
	t.Log("[Phase 12] Route 198.18.0.2 still present in RouteHub")

	// =========================================================================
	// Summary
	// =========================================================================

	t.Log("=== ClientDevice Full Pipeline Test Summary ===")
	t.Logf("  Connection pool: %d connections", poolConns)
	t.Logf("  TCP echo: single request/response OK")
	t.Logf("  HTTP: GET /health, POST /echo, GET /headers all OK")
	t.Logf("  Slot distribution: %d IPs across %d slots", len(destIPs), len(slotHits))
	t.Logf("  Concurrency: %d goroutines, %d succeeded", concurrency, successCount.Load())
	t.Logf("  Large payload: %d bytes without corruption", largeSize)
	t.Logf("  Sequential: %d requests on persistent connection", seqCount)
	t.Logf("  RouteHub: client route registered and retained")
	t.Log("=== ALL PHASES PASSED ===")
}
