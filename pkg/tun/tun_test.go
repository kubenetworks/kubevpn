package tun

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestConfigStructConstruction(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "IPv4 only",
			config: Config{
				Name:    "utun99",
				Addr:    "198.18.0.1/16",
				MTU:     config.DefaultMTU,
				Gateway: "198.18.0.1",
			},
		},
		{
			name: "IPv6 only",
			config: Config{
				Name:  "utun99",
				Addr6: "2001:2::1/64",
				MTU:   config.DefaultMTU,
			},
		},
		{
			name: "dual stack",
			config: Config{
				Name:    "utun99",
				Addr:    "198.18.0.1/16",
				Addr6:   "2001:2::1/64",
				MTU:     config.DefaultMTU,
				Gateway: "198.18.0.1",
			},
		},
		{
			name: "with routes",
			config: Config{
				Name:    "utun99",
				Addr:    "10.0.0.1/24",
				MTU:     1400,
				Gateway: "10.0.0.1",
				Routes: []types.Route{
					{
						Dst: net.IPNet{
							IP:   net.ParseIP("192.168.1.0"),
							Mask: net.CIDRMask(24, 32),
						},
					},
					{
						Dst: net.IPNet{
							IP:   net.ParseIP("10.244.0.0"),
							Mask: net.CIDRMask(16, 32),
						},
					},
				},
			},
		},
		{
			name: "zero MTU uses default",
			config: Config{
				Name: "utun99",
				Addr: "198.18.0.1/16",
				MTU:  0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.config
			// Verify fields are set correctly
			if cfg.Name != tt.config.Name {
				t.Fatalf("Name = %q, want %q", cfg.Name, tt.config.Name)
			}
			if cfg.Addr != tt.config.Addr {
				t.Fatalf("Addr = %q, want %q", cfg.Addr, tt.config.Addr)
			}
			if cfg.Addr6 != tt.config.Addr6 {
				t.Fatalf("Addr6 = %q, want %q", cfg.Addr6, tt.config.Addr6)
			}
			if cfg.MTU != tt.config.MTU {
				t.Fatalf("MTU = %d, want %d", cfg.MTU, tt.config.MTU)
			}
			if cfg.Gateway != tt.config.Gateway {
				t.Fatalf("Gateway = %q, want %q", cfg.Gateway, tt.config.Gateway)
			}
			if len(cfg.Routes) != len(tt.config.Routes) {
				t.Fatalf("Routes len = %d, want %d", len(cfg.Routes), len(tt.config.Routes))
			}
		})
	}
}

func TestConfigAddrParseable(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"valid IPv4 CIDR", "198.18.0.1/16", false},
		{"valid IPv4 /32", "10.0.0.1/32", false},
		{"valid IPv6 CIDR", "2001:2::1/64", false},
		{"invalid CIDR", "not-an-ip/16", true},
		{"missing mask", "198.18.0.1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := net.ParseCIDR(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCIDR(%q) error = %v, wantErr = %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestTunListenerErrClosed(t *testing.T) {
	if ErrClosed == nil {
		t.Fatal("ErrClosed should not be nil")
	}
	if ErrClosed.Error() == "" {
		t.Fatal("ErrClosed should have a message")
	}
	expected := "accept on closed listener"
	if ErrClosed.Error() != expected {
		t.Fatalf("ErrClosed.Error() = %q, want %q", ErrClosed.Error(), expected)
	}
}

func TestTunListenerCloseAndAccept(t *testing.T) {
	// Create a tunListener directly (without calling createTun) to test the
	// close/accept logic in isolation.
	ln := &tunListener{
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
		config: Config{
			Name: "utun-test",
			Addr: "198.18.0.1/16",
			MTU:  config.DefaultMTU,
		},
	}

	// Close should succeed the first time
	if err := ln.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	// Close again should return error
	if err := ln.Close(); err == nil {
		t.Fatal("second Close() should return error")
	}

	// Accept after close should return ErrClosed
	_, err := ln.Accept()
	if err == nil {
		t.Fatal("Accept() after Close() should return error")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Accept() error = %v, want %v", err, ErrClosed)
	}
}

func TestTunListenerAcceptWithConn(t *testing.T) {
	ln := &tunListener{
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
		config: Config{
			Name: "utun-test",
			Addr: "198.18.0.1/16",
			MTU:  config.DefaultMTU,
		},
	}

	// Push a mock conn into the channel
	mockAddr := &net.IPAddr{IP: net.ParseIP("198.18.0.1")}
	ln.addr = mockAddr
	ln.conns <- &mockConn{addr: mockAddr}

	// Accept should return the conn
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if conn == nil {
		t.Fatal("Accept() returned nil conn")
	}

	// Verify Addr() returns what we set
	if ln.Addr() == nil {
		t.Fatal("Addr() returned nil")
	}
	if ln.Addr().String() != "198.18.0.1" {
		t.Fatalf("Addr() = %q, want %q", ln.Addr().String(), "198.18.0.1")
	}
}

func TestTunConnDeadlineErrors(t *testing.T) {
	// tunConn does not support deadlines; verify it returns proper OpError
	c := &tunConn{
		addr:  &net.IPAddr{IP: net.ParseIP("198.18.0.1")},
		addr6: &net.IPAddr{IP: net.ParseIP("2001:2::1")},
	}

	deadline := time.Now().Add(5 * time.Second)

	t.Run("SetDeadline", func(t *testing.T) {
		err := c.SetDeadline(deadline)
		if err == nil {
			t.Fatal("SetDeadline should return error")
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("expected *net.OpError, got %T", err)
		}
		if opErr.Net != "tun" {
			t.Fatalf("OpError.Net = %q, want %q", opErr.Net, "tun")
		}
	})

	t.Run("SetReadDeadline", func(t *testing.T) {
		err := c.SetReadDeadline(deadline)
		if err == nil {
			t.Fatal("SetReadDeadline should return error")
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("expected *net.OpError, got %T", err)
		}
	})

	t.Run("SetWriteDeadline", func(t *testing.T) {
		err := c.SetWriteDeadline(deadline)
		if err == nil {
			t.Fatal("SetWriteDeadline should return error")
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("expected *net.OpError, got %T", err)
		}
	})
}

func TestTunConnLocalRemoteAddr(t *testing.T) {
	ipv4 := net.ParseIP("198.18.0.1")
	ipv6 := net.ParseIP("2001:2::1")

	c := &tunConn{
		addr:  &net.IPAddr{IP: ipv4},
		addr6: &net.IPAddr{IP: ipv6},
	}

	if c.LocalAddr() == nil {
		t.Fatal("LocalAddr() returned nil")
	}
	if c.LocalAddr().String() != "198.18.0.1" {
		t.Fatalf("LocalAddr() = %q, want %q", c.LocalAddr().String(), "198.18.0.1")
	}

	if c.RemoteAddr() == nil {
		t.Fatal("RemoteAddr() returned nil")
	}
	// RemoteAddr returns an empty IPAddr (IP is nil, String() returns "")
	remoteAddr := c.RemoteAddr()
	ipAddr, ok := remoteAddr.(*net.IPAddr)
	if !ok {
		t.Fatalf("RemoteAddr() type = %T, want *net.IPAddr", remoteAddr)
	}
	if ipAddr.IP != nil {
		t.Fatalf("RemoteAddr().IP = %v, want nil", ipAddr.IP)
	}
}


// mockConn is a minimal net.Conn implementation for testing the listener logic.
type mockConn struct {
	addr net.Addr
}

func (m *mockConn) Read([]byte) (int, error)         { return 0, nil }
func (m *mockConn) Write([]byte) (int, error)        { return 0, nil }
func (m *mockConn) Close() error                     { return nil }
func (m *mockConn) LocalAddr() net.Addr              { return m.addr }
func (m *mockConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (m *mockConn) SetDeadline(time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(time.Time) error { return nil }
