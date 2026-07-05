package util

import (
	"net"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CIDRsToString
// ---------------------------------------------------------------------------

func TestCIDRsToString(t *testing.T) {
	tests := []struct {
		name  string
		cidrs []*net.IPNet
		want  string
	}{
		{
			name:  "nil slice returns (none)",
			cidrs: nil,
			want:  "(none)",
		},
		{
			name:  "empty slice returns (none)",
			cidrs: []*net.IPNet{},
			want:  "(none)",
		},
		{
			name:  "single IPv4 CIDR",
			cidrs: []*net.IPNet{parseCIDRPure(t, "10.0.0.0/8")},
			want:  "10.0.0.0/8",
		},
		{
			name: "two IPv4 CIDRs comma-separated",
			cidrs: []*net.IPNet{
				parseCIDRPure(t, "10.0.0.0/8"),
				parseCIDRPure(t, "172.16.0.0/12"),
			},
			want: "10.0.0.0/8, 172.16.0.0/12",
		},
		{
			name: "multiple IPv4 CIDRs",
			cidrs: []*net.IPNet{
				parseCIDRPure(t, "10.0.0.0/8"),
				parseCIDRPure(t, "172.16.0.0/12"),
				parseCIDRPure(t, "192.168.0.0/16"),
			},
			want: "10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16",
		},
		{
			name: "IPv6 CIDR only",
			cidrs: []*net.IPNet{parseCIDRPure(t, "fd00::/112")},
			want:  "fd00::/112",
		},
		{
			name: "mixed IPv4 and IPv6 CIDRs",
			cidrs: []*net.IPNet{
				parseCIDRPure(t, "10.96.0.0/12"),
				parseCIDRPure(t, "fd00::/112"),
			},
			want: "10.96.0.0/12, fd00::/112",
		},
		{
			name: "slice containing a nil entry — nil skipped",
			cidrs: []*net.IPNet{
				parseCIDRPure(t, "10.0.0.0/8"),
				nil,
				parseCIDRPure(t, "192.168.0.0/16"),
			},
			// nil is skipped; result contains the two non-nil entries
			want: "10.0.0.0/8, 192.168.0.0/16",
		},
		{
			name:  "slice with only a nil entry returns empty join",
			cidrs: []*net.IPNet{nil},
			// len > 0 so not "(none)", but nil is skipped → parts is empty → Join("")
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CIDRsToString(tt.cidrs)
			if got != tt.want {
				t.Errorf("CIDRsToString() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCIDRFromFlag  (unexported, accessible within the package)
// ---------------------------------------------------------------------------

func TestParseCIDRFromFlag(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string // canonical CIDR strings; nil means empty result expected
	}{
		{
			name:    "empty string returns nothing",
			content: "",
			want:    nil,
		},
		{
			name:    "unrelated flag returns nothing",
			content: "--some-other-flag=10.0.0.0/8",
			want:    nil,
		},
		{
			name:    "cluster-cidr single IPv4",
			content: "--cluster-cidr=10.244.0.0/16",
			want:    []string{"10.244.0.0/16"},
		},
		{
			name:    "service-cluster-ip-range single IPv4",
			content: "--service-cluster-ip-range=10.96.0.0/12",
			want:    []string{"10.96.0.0/12"},
		},
		{
			name:    "cluster-cidr multiple CIDRs comma-separated",
			content: "--cluster-cidr=10.244.0.0/16,fd00::/112",
			want:    []string{"10.244.0.0/16", "fd00::/112"},
		},
		{
			name:    "service-cluster-ip-range multiple CIDRs",
			content: "--service-cluster-ip-range=10.96.0.0/12,fd01::/108",
			want:    []string{"10.96.0.0/12", "fd01::/108"},
		},
		{
			name:    "flag without equals sign returns nothing",
			content: "--cluster-cidr",
			want:    nil,
		},
		{
			name:    "flag with invalid CIDR after equals returns nothing",
			content: "--cluster-cidr=not-a-cidr",
			want:    nil,
		},
		{
			name:    "mixed valid and invalid CIDRs — only valid parsed",
			content: "--cluster-cidr=10.244.0.0/16,bad-cidr",
			want:    []string{"10.244.0.0/16"},
		},
		{
			name:    "pod-network-cidr flag name not matched",
			content: "--pod-network-cidr=10.0.0.0/8",
			want:    nil,
		},
		{
			// parseCIDRFromFlag uses strings.Cut at the first "=", so the after-part
			// becomes "192.168.0.0/16 --other=value" which net.ParseCIDR rejects
			// (contains space).  The function is designed for single-flag-per-line input
			// as produced by /proc/*/cmdline after tr "\0" "\n", not full command strings.
			name:    "full command line with embedded cluster-cidr flag returns nothing",
			content: "kube-controller-manager --cluster-cidr=192.168.0.0/16 --other=value",
			want:    nil,
		},
		{
			name:    "IPv6 only CIDR via cluster-cidr",
			content: "--cluster-cidr=fd12:3456:789a::/48",
			want:    []string{"fd12:3456:789a::/48"},
		},
		{
			name:    "cluster-cidr key present but value after equals is empty",
			content: "--cluster-cidr=",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCIDRFromFlag(tt.content)

			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Errorf("parseCIDRFromFlag(%q) = %v, want empty", tt.content, got)
				}
				return
			}

			if len(got) != len(tt.want) {
				t.Fatalf("parseCIDRFromFlag(%q): got %d CIDRs %v, want %d %v",
					tt.content, len(got), got, len(tt.want), tt.want)
			}
			for i, cidr := range got {
				if cidr.String() != tt.want[i] {
					t.Errorf("parseCIDRFromFlag(%q)[%d] = %q, want %q",
						tt.content, i, cidr.String(), tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetAPIServerIP
//
// This function does:
//   1. url.Parse — pure
//   2. net.SplitHostPort — pure
//   3. net.ParseIP — pure (IP-literal fast path)
//   4. net.LookupHost — network/DNS (impure for hostname inputs)
//
// We test only the cases where the host is already an IP literal, so
// LookupHost returns immediately without hitting a real DNS server.
// Hostname-based cases (e.g. "https://kubernetes.default.svc") require DNS
// and are therefore excluded from this no-cluster test file.
// ---------------------------------------------------------------------------

func TestGetAPIServerIP(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantIPs   []string // expected canonical IP strings; nil means we only check err
		wantErr   bool
		wantCount int // if > 0, assert exact count instead of comparing strings
	}{
		{
			name:    "IPv4 literal with port",
			input:   "https://10.0.0.1:6443",
			wantIPs: []string{"10.0.0.1"},
		},
		{
			name:    "IPv4 literal without port",
			input:   "https://10.0.0.1",
			wantIPs: []string{"10.0.0.1"},
		},
		{
			name:    "IPv6 literal with port bracket notation",
			input:   "https://[fd00::1]:6443",
			wantIPs: []string{"fd00::1"},
		},
		{
			name:    "loopback IPv4",
			input:   "https://127.0.0.1:443",
			wantIPs: []string{"127.0.0.1"},
		},
		{
			name:    "loopback IPv6",
			input:   "https://[::1]:443",
			wantIPs: []string{"::1"},
		},
		{
			name:    "HTTP scheme with IPv4 and port",
			input:   "http://192.168.1.100:8080",
			wantIPs: []string{"192.168.1.100"},
		},
		{
			name:    "raw IPv4 URL without scheme treated as path by url.Parse — host empty, no error",
			input:   "10.0.0.1",
			wantIPs: nil,
			// url.Parse("10.0.0.1") sets Path not Host; host is empty string; net.ParseIP("") == nil;
			// LookupHost("") may fail silently — no error from GetAPIServerIP, but 0 IPs
			wantCount: 0,
		},
		{
			name:    "malformed URL returns error",
			input:   "://bad url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetAPIServerIP(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Errorf("GetAPIServerIP(%q): expected error, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetAPIServerIP(%q): unexpected error: %v", tt.input, err)
			}

			// exact-count assertion (for cases where IPs are indeterminate but bounded)
			if tt.wantCount >= 0 && tt.wantIPs == nil {
				if len(got) != tt.wantCount {
					t.Errorf("GetAPIServerIP(%q): got %d IPs, want %d", tt.input, len(got), tt.wantCount)
				}
				return
			}

			// verify all expected IPs are present (LookupHost may add extras for hostname cases)
			gotSet := make(map[string]bool, len(got))
			for _, ip := range got {
				gotSet[ip.String()] = true
			}
			for _, want := range tt.wantIPs {
				if !gotSet[want] {
					t.Errorf("GetAPIServerIP(%q): IP %q not in result %v", tt.input, want, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetAPIServerIP — deduplication behaviour
// ---------------------------------------------------------------------------

// TestGetAPIServerIP_Deduplication confirms that when the same IP address
// appears more than once (e.g. from both ParseIP and LookupHost), the result
// contains it exactly once.
func TestGetAPIServerIP_Deduplication(t *testing.T) {
	// Use a loopback literal — LookupHost("127.0.0.1") typically returns the same IP.
	got, err := GetAPIServerIP("https://127.0.0.1:6443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := make(map[string]int)
	for _, ip := range got {
		seen[ip.String()]++
	}
	for ipStr, count := range seen {
		if count > 1 {
			t.Errorf("IP %q appeared %d times; want at most 1", ipStr, count)
		}
	}
	// The loopback IP must be present at least once.
	if seen["127.0.0.1"] < 1 {
		t.Errorf("expected 127.0.0.1 in result, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseCIDRPure is a test helper (separate name to avoid shadowing parseCIDR
// from cidr_test.go which lives in the same package).
func parseCIDRPure(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", s, err)
	}
	return ipNet
}

// TestCIDRsToString_OrderPreserved verifies that the output order matches
// the input slice order (no sorting is applied by the function).
func TestCIDRsToString_OrderPreserved(t *testing.T) {
	cidrs := []*net.IPNet{
		parseCIDRPure(t, "192.168.0.0/16"),
		parseCIDRPure(t, "10.0.0.0/8"),
		parseCIDRPure(t, "172.16.0.0/12"),
	}
	got := CIDRsToString(cidrs)
	parts := strings.Split(got, ", ")
	want := []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"}
	if len(parts) != len(want) {
		t.Fatalf("got %d parts %v, want %d %v", len(parts), parts, len(want), want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("part[%d] = %q, want %q", i, parts[i], want[i])
		}
	}
}
