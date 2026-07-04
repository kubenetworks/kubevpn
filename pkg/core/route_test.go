package core

import (
	"fmt"
	"net"
	"testing"
)

func TestParseRoutes_ValidCIDRs(t *testing.T) {
	routes := parseRoutes("10.0.0.0/8,192.168.0.0/16")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
	if routes[0].Dst.String() != "10.0.0.0/8" {
		t.Fatalf("expected 10.0.0.0/8, got %s", routes[0].Dst.String())
	}
	if routes[1].Dst.String() != "192.168.0.0/16" {
		t.Fatalf("expected 192.168.0.0/16, got %s", routes[1].Dst.String())
	}
}

func TestParseRoutes_WithSpaces(t *testing.T) {
	routes := parseRoutes(" 10.0.0.0/8 , 172.16.0.0/12 ")
	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routes))
	}
}

func TestParseRoutes_Empty(t *testing.T) {
	routes := parseRoutes("")
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes, got %d", len(routes))
	}
}

func TestParseRoutes_InvalidCIDR(t *testing.T) {
	routes := parseRoutes("invalid,10.0.0.0/8")
	if len(routes) != 1 {
		t.Fatalf("expected 1 valid route, got %d", len(routes))
	}
}

func TestParseForwarder_ValidRemote(t *testing.T) {
	fwd, err := ParseForwarder("tcp://127.0.0.1:8422")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fwd == nil {
		t.Fatal("expected non-nil forwarder")
	}
}

func TestParseForwarder_EmptyRemote(t *testing.T) {
	_, err := ParseForwarder("")
	if err == nil {
		t.Fatal("expected error for empty remote")
	}
}

func TestGenerateServers_UnknownProtocol(t *testing.T) {
	_, err := GenerateServers([]string{"unknown://:1234"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown protocol")
	}
}

func TestGenerateServers_EmptyListeners(t *testing.T) {
	_, err := GenerateServers([]string{}, nil)
	if err == nil {
		t.Fatal("expected error for empty listeners")
	}
}

func TestGenerateServers_InvalidNode(t *testing.T) {
	_, err := GenerateServers([]string{""}, nil)
	if err == nil {
		t.Fatal("expected error for empty listener string")
	}
}

func TestGenerateServers_NilHub_UsesDefault(t *testing.T) {
	// Should not panic with nil hub
	_, _ = GenerateServers([]string{"unknown://:1234"}, nil)
}

func TestRegisterProtocol_CustomProtocol(t *testing.T) {
	called := false
	RegisterProtocol("test-proto", func(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
		called = true
		return nil, nil, fmt.Errorf("test factory called")
	})
	defer delete(protocolRegistry, "test-proto")

	_, err := GenerateServers([]string{"test-proto://:9999"}, nil)
	if err == nil {
		t.Fatal("expected error from test factory")
	}
	if !called {
		t.Fatal("custom protocol factory was not called")
	}
}

func TestProtocolRegistry_AllProtocolsRegistered(t *testing.T) {
	expected := []string{"tun", "gtcp", "gudp", "ssh"}
	for _, proto := range expected {
		if _, ok := protocolRegistry[proto]; !ok {
			t.Fatalf("protocol %q not registered", proto)
		}
	}
}
