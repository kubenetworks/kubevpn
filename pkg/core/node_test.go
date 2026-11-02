package core

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Node tests
// ---------------------------------------------------------------------------

func TestNodeParseBasic(t *testing.T) {
	tests := []struct {
		input    string
		protocol string
		addr     string
		forward  string
	}{
		{"gtcp://:10801", "gtcp", ":10801", ""},
		{"gudp://:10802", "gudp", ":10802", ""},
		{"ssh://192.168.1.1:22", "ssh", "192.168.1.1:22", ""},
		{"tun://", "tun", "", ""},
		{"tun:///tcp://10.0.0.1:8422", "tun", "", "tcp://10.0.0.1:8422"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			node, err := ParseNode(tt.input)
			if err != nil {
				t.Fatalf("ParseNode(%q) error: %v", tt.input, err)
			}
			if node.Protocol != tt.protocol {
				t.Errorf("Protocol = %q, want %q", node.Protocol, tt.protocol)
			}
			if node.Addr != tt.addr {
				t.Errorf("Addr = %q, want %q", node.Addr, tt.addr)
			}
			if node.Forward != tt.forward {
				t.Errorf("Forward = %q, want %q", node.Forward, tt.forward)
			}
		})
	}
}

func TestNodeParseWithParams(t *testing.T) {
	input := "tun://?net=198.18.0.100/16&mtu=1500&name=utun9"
	node, err := ParseNode(input)
	if err != nil {
		t.Fatalf("ParseNode error: %v", err)
	}
	if node.Protocol != "tun" {
		t.Errorf("Protocol = %q, want tun", node.Protocol)
	}
	if node.Get("net") != "198.18.0.100/16" {
		t.Errorf("net param = %q, want 198.18.0.100/16", node.Get("net"))
	}
	if node.GetInt("mtu") != 1500 {
		t.Errorf("mtu param = %d, want 1500", node.GetInt("mtu"))
	}
	if node.Get("name") != "utun9" {
		t.Errorf("name param = %q, want utun9", node.Get("name"))
	}
}

func TestNodeParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"bare path no colon", "/some/path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseNode(tt.input)
			if err == nil {
				t.Fatalf("ParseNode(%q) should return error", tt.input)
			}
		})
	}
}

func TestNodeStringRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		node *Node
	}{
		{
			"simple gtcp",
			NewNode("gtcp", ":10801"),
		},
		{
			"tun with params",
			NewNode("tun", "").
				WithParam("net", "198.18.0.100/16").
				WithParam("mtu", "1500"),
		},
		{
			"tun with forward",
			NewNode("tun", "").
				WithForward("tcp://10.0.0.1:8422"),
		},
		{
			"ssh with address",
			NewNode("ssh", "192.168.1.1:22"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.node.String()
			parsed, err := ParseNode(s)
			if err != nil {
				t.Fatalf("ParseNode(%q) error: %v", s, err)
			}
			if parsed.Protocol != tt.node.Protocol {
				t.Errorf("Protocol mismatch: got %q, want %q", parsed.Protocol, tt.node.Protocol)
			}
			if parsed.Addr != tt.node.Addr {
				t.Errorf("Addr mismatch: got %q, want %q", parsed.Addr, tt.node.Addr)
			}
			// Compare query values
			for key, vals := range tt.node.Values {
				for i, v := range vals {
					parsedVals := parsed.Values[key]
					if i >= len(parsedVals) || parsedVals[i] != v {
						t.Errorf("Values[%q][%d] mismatch: got %q, want %q",
							key, i, parsedVals, v)
					}
				}
			}
		})
	}
}

func TestNodeGetAndGetInt(t *testing.T) {
	node := NewNode("tun", "").
		WithParam("mtu", "9000").
		WithParam("empty", "")

	if node.GetInt("mtu") != 9000 {
		t.Errorf("GetInt(mtu) = %d, want 9000", node.GetInt("mtu"))
	}
	if node.GetInt("missing") != 0 {
		t.Errorf("GetInt(missing) = %d, want 0", node.GetInt("missing"))
	}
	if node.GetInt("empty") != 0 {
		t.Errorf("GetInt(empty) = %d, want 0", node.GetInt("empty"))
	}
	if node.Get("missing") != "" {
		t.Errorf("Get(missing) = %q, want empty", node.Get("missing"))
	}
}

func TestNodeNewNodeBuilderPattern(t *testing.T) {
	node := NewNode("tun", "").
		WithForward("tcp://10.0.0.1:8422").
		WithParam("net", "198.18.0.102/16").
		WithParam("route", "10.233.0.0/16,10.233.64.0/18")

	if node.Protocol != "tun" {
		t.Errorf("Protocol = %q, want tun", node.Protocol)
	}
	if node.Forward != "tcp://10.0.0.1:8422" {
		t.Errorf("Forward = %q, want tcp://10.0.0.1:8422", node.Forward)
	}
	if node.Get("route") != "10.233.0.0/16,10.233.64.0/18" {
		t.Errorf("route = %q", node.Get("route"))
	}
}
