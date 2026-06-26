package core

import (
	"context"
	"net"
	"testing"
)

func TestParseNode_Valid(t *testing.T) {
	node, err := ParseNode("tcp://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Addr != "127.0.0.1:8080" {
		t.Fatalf("expected addr 127.0.0.1:8080, got %s", node.Addr)
	}
	if node.Protocol != "tcp" {
		t.Fatalf("expected protocol tcp, got %s", node.Protocol)
	}
}

func TestParseNode_WithForward(t *testing.T) {
	node, err := ParseNode("tun:/tcp://10.0.0.1:8422?net=198.18.0.1/16")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Protocol != "tun" {
		t.Fatalf("expected protocol tun, got %s", node.Protocol)
	}
	if node.Forward != "tcp://10.0.0.1:8422" {
		t.Fatalf("expected forward tcp://10.0.0.1:8422, got %s", node.Forward)
	}
	if node.Get("net") != "198.18.0.1/16" {
		t.Fatalf("expected net=198.18.0.1/16, got %s", node.Get("net"))
	}
}

func TestParseNode_Empty(t *testing.T) {
	_, err := ParseNode("")
	if err == nil {
		t.Fatal("expected error for empty node")
	}
}

func TestParseNode_NoScheme(t *testing.T) {
	_, err := ParseNode("127.0.0.1:8080")
	if err == nil {
		t.Fatal("expected error for missing protocol scheme")
	}
}

func TestNewNode_Basic(t *testing.T) {
	node := NewNode("gtcp", ":10801")
	if node.Protocol != "gtcp" {
		t.Fatalf("expected protocol gtcp, got %s", node.Protocol)
	}
	if node.Addr != ":10801" {
		t.Fatalf("expected addr :10801, got %s", node.Addr)
	}
	if node.Values == nil {
		t.Fatal("Values should not be nil")
	}
}

func TestNewNode_WithForward(t *testing.T) {
	node := NewNode("tun", "").WithForward("tcp://10.0.0.1:8422")
	if node.Forward != "tcp://10.0.0.1:8422" {
		t.Fatalf("expected forward tcp://10.0.0.1:8422, got %s", node.Forward)
	}
}

func TestNewNode_WithParams(t *testing.T) {
	node := NewNode("tun", "").
		WithParam("net", "198.18.0.100/16").
		WithParam("mtu", "1500")
	if node.Get("net") != "198.18.0.100/16" {
		t.Fatalf("expected net=198.18.0.100/16, got %s", node.Get("net"))
	}
	if node.GetInt("mtu") != 1500 {
		t.Fatalf("expected mtu=1500, got %d", node.GetInt("mtu"))
	}
}

func TestNewNode_Chaining(t *testing.T) {
	node := NewNode("tun", "").
		WithForward("tcp://127.0.0.1:8422").
		WithParam("net", "198.18.0.102/16").
		WithParam("route", "198.18.0.0/16,10.0.0.0/8")
	if node.Protocol != "tun" {
		t.Fatalf("expected protocol tun, got %s", node.Protocol)
	}
	if node.Forward != "tcp://127.0.0.1:8422" {
		t.Fatalf("unexpected forward: %s", node.Forward)
	}
	if node.Get("net") != "198.18.0.102/16" {
		t.Fatalf("unexpected net: %s", node.Get("net"))
	}
	if node.Get("route") != "198.18.0.0/16,10.0.0.0/8" {
		t.Fatalf("unexpected route: %s", node.Get("route"))
	}
}

func TestNode_String(t *testing.T) {
	tests := []struct {
		name string
		node *Node
		want string
	}{
		{
			name: "simple",
			node: NewNode("gtcp", ":10801"),
			want: "gtcp://:10801",
		},
		{
			name: "with forward",
			node: NewNode("tun", "").WithForward("tcp://10.0.0.1:8422"),
			want: "tun:///tcp://10.0.0.1:8422",
		},
		{
			name: "with params",
			node: NewNode("tun", "").WithParam("net", "198.18.0.100/16"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.node.String()
			if s == "" {
				t.Fatal("String() should not be empty")
			}
			if tt.want != "" && s != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, s)
			}
		})
	}
}

func TestGenerateServersFromNodes_Empty(t *testing.T) {
	_, err := GenerateServersFromNodes(nil, nil)
	if err == nil {
		t.Fatal("expected error for nil nodes")
	}
}

func TestGenerateServersFromNodes_UnknownProtocol(t *testing.T) {
	_, err := GenerateServersFromNodes([]*Node{NewNode("unknown", ":1234")}, nil)
	if err == nil {
		t.Fatal("expected error for unknown protocol")
	}
}

func TestParseNode_QueryParams(t *testing.T) {
	node, err := ParseNode("tun://?net=198.18.0.100/16&mtu=1500&name=utun9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node.Get("net") != "198.18.0.100/16" {
		t.Fatalf("unexpected net: %s", node.Get("net"))
	}
	if node.GetInt("mtu") != 1500 {
		t.Fatalf("unexpected mtu: %d", node.GetInt("mtu"))
	}
	if node.Get("name") != "utun9" {
		t.Fatalf("unexpected name: %s", node.Get("name"))
	}
}

func TestForwarder_IsEmpty_Nil(t *testing.T) {
	var f *Forwarder
	if !f.IsEmpty() {
		t.Fatal("nil forwarder should be empty")
	}
}

func TestForwarder_IsEmpty_NoAddr(t *testing.T) {
	f := &Forwarder{}
	if !f.IsEmpty() {
		t.Fatal("forwarder with empty addr should be empty")
	}
}

func TestForwarder_IsEmpty_WithAddr(t *testing.T) {
	f := &Forwarder{Addr: "127.0.0.1:8080"}
	if f.IsEmpty() {
		t.Fatal("forwarder with addr should not be empty")
	}
}

func TestForwarder_DialContext_Empty(t *testing.T) {
	f := &Forwarder{}
	_, err := f.DialContext(context.Background())
	if err != errEmptyForwarder {
		t.Fatalf("expected errEmptyForwarder, got %v", err)
	}
}

type mockTransporter struct {
	dialFn func(ctx context.Context, addr string) (net.Conn, error)
}

func (m *mockTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return m.dialFn(ctx, addr)
}

type mockConnector struct {
	connectFn func(ctx context.Context, conn net.Conn) (net.Conn, error)
}

func (m *mockConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return m.connectFn(ctx, conn)
}

func TestForwarder_DialContext_Success(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	f := &Forwarder{
		Addr:       "127.0.0.1:9999",
		MaxRetries: 1,
		Transporter: &mockTransporter{
			dialFn: func(ctx context.Context, addr string) (net.Conn, error) {
				if addr != "127.0.0.1:9999" {
					t.Fatalf("unexpected addr: %s", addr)
				}
				return client, nil
			},
		},
		Connector: &mockConnector{
			connectFn: func(ctx context.Context, conn net.Conn) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	conn, err := f.DialContext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
}

func TestForwarder_DialContext_RetryOnError(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	attempts := 0
	f := &Forwarder{
		Addr:       "127.0.0.1:9999",
		MaxRetries: 3,
		Transporter: &mockTransporter{
			dialFn: func(ctx context.Context, addr string) (net.Conn, error) {
				attempts++
				if attempts < 3 {
					return nil, net.ErrClosed
				}
				return client, nil
			},
		},
		Connector: &mockConnector{
			connectFn: func(ctx context.Context, conn net.Conn) (net.Conn, error) {
				return conn, nil
			},
		},
	}

	conn, err := f.DialContext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}
