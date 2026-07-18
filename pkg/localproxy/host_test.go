package localproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// TestHostConnector_DialsRealListener verifies HostConnector reaches a real TCP listener
// and round-trips bytes (basic direct-dial sanity).
func TestHostConnector_DialsRealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn) // echo
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	conn, err := NewHostConnector().Connect(context.Background(), host, port)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo mismatch: got %q", buf)
	}
}

// TestHostConnector_ResolvesHostnameViaHost proves the socks5h property: a hostname
// ("localhost") is resolved by the host resolver, not by the client, and dialed directly.
func TestHostConnector_ResolvesHostnameViaHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	conn, err := NewHostConnector().Connect(context.Background(), "localhost", port)
	if err != nil {
		t.Fatalf("Connect to localhost:%d via host resolver failed: %v", port, err)
	}
	_ = conn.Close()
}

// TestHostConnector_UnreachableFailsWithinTimeout verifies a dial to a closed port returns
// an error promptly rather than hanging.
func TestHostConnector_UnreachableFailsWithinTimeout(t *testing.T) {
	// Grab a port then release it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	start := time.Now()
	conn, err := NewHostConnector().Connect(context.Background(), "127.0.0.1", port)
	if err == nil {
		conn.Close()
		t.Fatal("expected error dialing closed port, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("dial to closed port took %s, expected prompt failure", elapsed)
	}
}

// TestHostConnector_SOCKS5EndToEnd wires a real localproxy.Server with a HostConnector and
// drives an HTTP request through it via a SOCKS5 client, proving host-direct socks5h egress
// end-to-end: the client hands the proxy a hostname, the proxy resolves+dials it on the host.
func TestHostConnector_SOCKS5EndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	// backend.URL is http://127.0.0.1:<port>; rewrite host to "localhost" so the proxy must
	// resolve the name itself (socks5h).
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = serveSOCKS5(ctx, ln, NewHostConnector()) }()

	dialer, err := xproxy.SOCKS5("tcp", ln.Addr().String(), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
	}

	resp, err := httpClient.Get("http://localhost:" + portStr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", string(body))
	}
}
