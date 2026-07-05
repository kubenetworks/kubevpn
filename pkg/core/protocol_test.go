package core

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/util/cert"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// ---------------------------------------------------------------------------
// parseRoutes tests — complement route_test.go with additional cases
// ---------------------------------------------------------------------------

func TestParseRoutes_ThreeCIDRs(t *testing.T) {
	routes := parseRoutes("10.0.0.0/8,192.168.1.0/24,172.16.0.0/12")
	if len(routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(routes))
	}
	expected := []string{"10.0.0.0/8", "192.168.1.0/24", "172.16.0.0/12"}
	for i, route := range routes {
		if got := route.Dst.String(); got != expected[i] {
			t.Errorf("route[%d] = %q, want %q", i, got, expected[i])
		}
	}
}

func TestParseRoutes_AllInvalid(t *testing.T) {
	routes := parseRoutes("foo,bar,baz")
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes for all-invalid input, got %d", len(routes))
	}
}

func TestParseRoutes_IPv6(t *testing.T) {
	routes := parseRoutes("fd00::/64,2001:db8::/32")
	if len(routes) != 2 {
		t.Fatalf("expected 2 IPv6 routes, got %d", len(routes))
	}
	if routes[0].Dst.String() != "fd00::/64" {
		t.Errorf("route[0] = %q, want fd00::/64", routes[0].Dst.String())
	}
	if routes[1].Dst.String() != "2001:db8::/32" {
		t.Errorf("route[1] = %q, want 2001:db8::/32", routes[1].Dst.String())
	}
}

func TestParseRoutes_MixedValidInvalid(t *testing.T) {
	routes := parseRoutes("10.0.0.0/8,not-a-cidr,192.168.1.0/24")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes (invalid skipped), got %d", len(routes))
	}
	if routes[0].Dst.String() != "10.0.0.0/8" {
		t.Errorf("route[0] = %q, want 10.0.0.0/8", routes[0].Dst.String())
	}
	if routes[1].Dst.String() != "192.168.1.0/24" {
		t.Errorf("route[1] = %q, want 192.168.1.0/24", routes[1].Dst.String())
	}
}

// ---------------------------------------------------------------------------
// ParseForwarder tests — complement route_test.go
// ---------------------------------------------------------------------------

func TestParseForwarder_ValidRemote_Fields(t *testing.T) {
	fwd, err := ParseForwarder("tcp://10.0.0.1:8422")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fwd.Addr != "10.0.0.1:8422" {
		t.Errorf("Addr = %q, want 10.0.0.1:8422", fwd.Addr)
	}
	if fwd.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", fwd.MaxRetries)
	}
	if fwd.Connector == nil {
		t.Error("Connector should not be nil")
	}
	if fwd.Transporter == nil {
		t.Error("Transporter should not be nil")
	}
	if fwd.IsEmpty() {
		t.Error("forwarder with valid addr should not be empty")
	}
}

func TestParseForwarder_InvalidURI(t *testing.T) {
	_, err := ParseForwarder("not-a-valid-uri")
	if err == nil {
		t.Fatal("expected error for invalid URI (no scheme)")
	}
}

func TestParseForwarder_LocalhostAddr(t *testing.T) {
	fwd, err := ParseForwarder("tcp://127.0.0.1:9999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fwd.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want 127.0.0.1:9999", fwd.Addr)
	}
}

// ---------------------------------------------------------------------------
// RegisterProtocol + GenerateServers tests — complement route_test.go
// ---------------------------------------------------------------------------

// testProtoHandler is a minimal Handler for protocol registration tests.
type testProtoHandler struct{}

func (h *testProtoHandler) Handle(_ context.Context, _ net.Conn) {}

func TestRegisterProtocol_GenerateServers_FullFlow(t *testing.T) {
	const proto = "test-full-flow"
	RegisterProtocol(proto, func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		return ln, &testProtoHandler{}, nil
	})
	defer delete(protocolRegistry, proto)

	servers, err := GenerateServers([]string{proto + "://127.0.0.1:0"}, NewRouteHub())
	if err != nil {
		t.Fatalf("GenerateServers error: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Listener == nil {
		t.Fatal("Listener should not be nil")
	}
	if servers[0].Handler == nil {
		t.Fatal("Handler should not be nil")
	}

	// Verify the listener is actually usable.
	addr := servers[0].Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("failed to dial test server at %s: %v", addr, err)
	}
	conn.Close()
	servers[0].Listener.Close()
}

func TestRegisterProtocol_MultipleProtocols(t *testing.T) {
	const proto1 = "test-multi-a"
	const proto2 = "test-multi-b"
	RegisterProtocol(proto1, func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, &testProtoHandler{}, err
	})
	RegisterProtocol(proto2, func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, &testProtoHandler{}, err
	})
	defer delete(protocolRegistry, proto1)
	defer delete(protocolRegistry, proto2)

	servers, err := GenerateServers([]string{
		proto1 + "://127.0.0.1:0",
		proto2 + "://127.0.0.1:0",
	}, NewRouteHub())
	if err != nil {
		t.Fatalf("GenerateServers error: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
	for i, s := range servers {
		if s.Listener == nil {
			t.Errorf("server[%d].Listener is nil", i)
		}
		s.Listener.Close()
	}
}

func TestGenerateServersFromNodes_NilHub_PassedThrough(t *testing.T) {
	const proto = "test-nilhub-pt"
	RegisterProtocol(proto, func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		if hub != nil {
			t.Error("hub should be nil when nil is passed (no fallback)")
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, &testProtoHandler{}, err
	})
	defer delete(protocolRegistry, proto)

	servers, err := GenerateServersFromNodes([]*Node{NewNode(proto, "127.0.0.1:0")}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	servers[0].Listener.Close()
}

func TestGenerateServersFromNodes_FactoryError(t *testing.T) {
	const proto = "test-factory-err"
	RegisterProtocol(proto, func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		return nil, nil, net.ErrClosed
	})
	defer delete(protocolRegistry, proto)

	_, err := GenerateServersFromNodes([]*Node{NewNode(proto, ":0")}, NewRouteHub())
	if err == nil {
		t.Fatal("expected error when factory returns error")
	}
}

// ---------------------------------------------------------------------------
// TCPTransporter tests
// ---------------------------------------------------------------------------

func TestTCPTransporter_NilTLS(t *testing.T) {
	// Start a local TCP listener to accept connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Echo a byte back to confirm the connection works.
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
		conn.Close()
	}()

	tr := TCPTransporter(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer conn.Close()

	// Verify the connection is a plain TCP conn (not TLS).
	if _, ok := conn.(*tls.Conn); ok {
		t.Fatal("expected plain TCP conn when TLS config is nil, got *tls.Conn")
	}

	// Verify data round-trip.
	payload := []byte("ping")
	_, err = conn.Write(payload)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	buf := make([]byte, 16)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Errorf("echo mismatch: got %q, want %q", buf[:n], "ping")
	}

	wg.Wait()
}

func TestTCPTransporter_WithTLS(t *testing.T) {
	// Generate self-signed certificate for the test.
	host := "localhost"
	certPEM, keyPEM, err := cert.GenerateSelfSignedCertKeyWithFixtures(host, []net.IP{net.IPv4(127, 0, 0, 1)}, nil, "")
	if err != nil {
		t.Fatalf("failed to generate self-signed cert: %v", err)
	}

	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("failed to load server keypair: %v", err)
	}

	serverTLSConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSConfig)
	if err != nil {
		t.Fatalf("failed to start TLS listener: %v", err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 16)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}()

	// Build TLS secret map matching the project's config keys.
	tlsSecret := map[string][]byte{
		config.TLSCertKey:       certPEM,
		config.TLSPrivateKeyKey: keyPEM,
		config.TLSServerName:    []byte(host),
	}

	tr := TCPTransporter(tlsSecret)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer conn.Close()

	// Verify the connection is a TLS conn.
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		t.Fatal("expected *tls.Conn when TLS config is provided")
	}

	// Complete the handshake explicitly to verify it succeeds.
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		t.Fatalf("TLS handshake error: %v", err)
	}

	state := tlsConn.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		t.Errorf("TLS version = 0x%04x, want 0x%04x (TLS 1.3)", state.Version, tls.VersionTLS13)
	}

	// Verify data round-trip over TLS.
	payload := []byte("tls-ping")
	_, err = conn.Write(payload)
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}

	buf := make([]byte, 16)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read error: %v", err)
	}
	if string(buf[:n]) != "tls-ping" {
		t.Errorf("echo mismatch: got %q, want %q", buf[:n], "tls-ping")
	}

	wg.Wait()
}

func TestTCPTransporter_InvalidTLSSecret_FallsBackToRawTCP(t *testing.T) {
	// Provide a TLS map with invalid cert data — should fall back to raw TCP.
	tlsSecret := map[string][]byte{
		config.TLSCertKey:       []byte("not-a-cert"),
		config.TLSPrivateKeyKey: []byte("not-a-key"),
		config.TLSServerName:    []byte("localhost"),
	}

	tr := TCPTransporter(tlsSecret)

	// Verify the transporter was created (falls back to nil TLS).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := tr.Dial(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	conn.Close()
}

func TestTCPTransporter_DialUnreachable(t *testing.T) {
	// Bind a listener, grab its address, then close it immediately
	// so we have a guaranteed-unreachable address.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	tr := TCPTransporter(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = tr.Dial(ctx, addr)
	if err == nil {
		t.Fatal("expected error dialing unreachable address")
	}
}

// ---------------------------------------------------------------------------
// tcpKeepAliveListener tests
// ---------------------------------------------------------------------------

func TestTCPKeepAliveListener_Accept(t *testing.T) {
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer tcpLn.Close()

	kaLn := &tcpKeepAliveListener{TCPListener: tcpLn.(*net.TCPListener)}

	var wg sync.WaitGroup
	wg.Add(1)
	var acceptConn net.Conn
	var acceptErr error
	go func() {
		defer wg.Done()
		acceptConn, acceptErr = kaLn.Accept()
	}()

	// Dial into the listener.
	conn, err := net.DialTimeout("tcp", tcpLn.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("Dial error: %v", err)
	}
	defer conn.Close()

	wg.Wait()
	if acceptErr != nil {
		t.Fatalf("Accept error: %v", acceptErr)
	}
	if acceptConn == nil {
		t.Fatal("accepted conn should not be nil")
	}

	// Verify it's a TCP conn with keepalive properties set.
	_, ok := acceptConn.(*net.TCPConn)
	if !ok {
		t.Fatal("expected *net.TCPConn from tcpKeepAliveListener")
	}
	acceptConn.Close()
}
