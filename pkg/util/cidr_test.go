package util

import (
	"net"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestRemoveCIDRsContainingIPs(t *testing.T) {
	tests := []struct {
		name          string
		cidrStrings   []string
		ipStrings     []string
		expectedCIDRs []string
		expectPanic   bool
	}{
		{
			name: "Normal case - some overlaps",
			cidrStrings: []string{
				"10.140.45.0/24", "10.140.44.0/24", "10.31.0.0/24", "10.31.1.0/24", "10.31.2.0/24", "10.31.3.0/24", "10.140.47.0/24", "10.140.46.0/24",
			},
			ipStrings: []string{
				"10.140.45.1", "10.140.46.220", "10.140.45.180", "10.140.45.152",
				"10.140.46.183", "10.140.45.52", "10.140.47.148", "10.140.46.214",
			},
			expectedCIDRs: []string{
				"10.140.44.0/24", "10.31.0.0/24", "10.31.1.0/24", "10.31.2.0/24", "10.31.3.0/24",
			},
			expectPanic: false,
		},
		{
			name:        "Empty CIDR list",
			cidrStrings: []string{},
			ipStrings: []string{
				"10.140.45.1",
			},
			expectedCIDRs: []string{},
			expectPanic:   false,
		},
		{
			name: "Empty IP list",
			cidrStrings: []string{
				"10.140.45.0/24", "10.140.44.0/24",
			},
			ipStrings: []string{},
			expectedCIDRs: []string{
				"10.140.45.0/24", "10.140.44.0/24",
			},
			expectPanic: false,
		},
		{
			name: "All CIDRs removed",
			cidrStrings: []string{
				"10.140.45.0/24", "10.140.46.0/24",
			},
			ipStrings: []string{
				"10.140.45.1", "10.140.46.220",
			},
			expectedCIDRs: []string{},
			expectPanic:   false,
		},
		{
			name: "Overlapping CIDRs",
			cidrStrings: []string{
				"10.140.45.0/24", "10.140.45.0/25", "10.140.45.128/25",
			},
			ipStrings: []string{
				"10.140.45.1", "10.140.45.129",
			},
			expectedCIDRs: []string{},
			expectPanic:   false,
		},
		{
			name: "Invalid CIDR format",
			cidrStrings: []string{
				"10.140.45.0/24", "invalid-cidr",
			},
			ipStrings: []string{
				"10.140.45.1",
			},
			expectedCIDRs: nil, // Panic expected
			expectPanic:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					if !test.expectPanic {
						t.Errorf("unexpected panic: %v", r)
					}
				} else if test.expectPanic {
					t.Errorf("expected panic but got none")
				}
			}()

			var cidrs []*net.IPNet
			for _, cidr := range test.cidrStrings {
				_, ipNet, err := net.ParseCIDR(cidr)
				if err != nil {
					if test.expectPanic {
						panic(err)
					}
					t.Fatalf("failed to parse CIDR %s: %v", cidr, err)
				}
				cidrs = append(cidrs, ipNet)
			}

			var ipList []net.IP
			for _, ip := range test.ipStrings {
				parsedIP := net.ParseIP(ip)
				if parsedIP == nil {
					t.Fatalf("failed to parse IP %s", ip)
				}
				ipList = append(ipList, parsedIP)
			}

			cidrs = RemoveCIDRsContainingIPs(cidrs, ipList)
			if !test.expectPanic {
				if len(cidrs) != len(test.expectedCIDRs) {
					t.Fatalf("unexpected number of remaining CIDRs: got %d, want %d", len(cidrs), len(test.expectedCIDRs))
				}
				for i, cidr := range cidrs {
					if cidr.String() != test.expectedCIDRs[i] {
						t.Errorf("unexpected CIDR at index %d: got %s, want %s", i, cidr.String(), test.expectedCIDRs[i])
					}
				}
			}
		})
	}
}

func TestRemoveLargerOverlappingCIDRs(t *testing.T) {
	type args struct {
		cidrNets []*net.IPNet
	}
	tests := []struct {
		name string
		args args
		want []*net.IPNet
	}{
		{
			name: "equal",
			args: args{
				cidrNets: []*net.IPNet{
					{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
					{IP: net.ParseIP("192.168.2.0"), Mask: net.CIDRMask(24, 32)},
				}},
			want: []*net.IPNet{
				{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
				{IP: net.ParseIP("192.168.2.0"), Mask: net.CIDRMask(24, 32)},
			},
		},
		{
			name: "larger",
			args: args{
				cidrNets: []*net.IPNet{
					{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
					{IP: net.ParseIP("192.168.2.0"), Mask: net.CIDRMask(16, 32)},
				}},
			want: []*net.IPNet{
				{IP: net.ParseIP("192.168.2.0"), Mask: net.CIDRMask(16, 32)},
			},
		},
		{
			name: "deduplicated",
			args: args{
				cidrNets: []*net.IPNet{
					{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
					{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
				}},
			want: []*net.IPNet{
				{IP: net.ParseIP("192.168.1.0"), Mask: net.CIDRMask(24, 32)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RemoveLargerOverlappingCIDRs(tt.args.cidrNets); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RemoveLargerOverlappingCIDRs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveLargerOverlappingCIDRs_NonOverlapping(t *testing.T) {
	// Completely disjoint CIDRs should all be preserved
	cidrs := []*net.IPNet{
		parseCIDR(t, "10.0.0.0/8"),
		parseCIDR(t, "172.16.0.0/12"),
		parseCIDR(t, "192.168.0.0/16"),
	}
	got := RemoveLargerOverlappingCIDRs(cidrs)
	if len(got) != 3 {
		t.Fatalf("expected 3 non-overlapping CIDRs preserved, got %d: %v", len(got), got)
	}
}

func TestRemoveLargerOverlappingCIDRs_NestedMultipleLevels(t *testing.T) {
	// 10.0.0.0/8 contains 10.1.0.0/16 which contains 10.1.1.0/24
	// Only the largest (smallest mask) should survive
	cidrs := []*net.IPNet{
		parseCIDR(t, "10.1.1.0/24"),
		parseCIDR(t, "10.0.0.0/8"),
		parseCIDR(t, "10.1.0.0/16"),
	}
	got := RemoveLargerOverlappingCIDRs(cidrs)
	if len(got) != 1 {
		t.Fatalf("expected 1 CIDR after removing nested overlaps, got %d: %v", len(got), got)
	}
	if got[0].String() != "10.0.0.0/8" {
		t.Fatalf("expected 10.0.0.0/8, got %s", got[0].String())
	}
}

func TestRemoveLargerOverlappingCIDRs_EmptyInput(t *testing.T) {
	got := RemoveLargerOverlappingCIDRs(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty result for nil input, got %v", got)
	}
}

func TestRemoveLargerOverlappingCIDRs_SingleCIDR(t *testing.T) {
	cidrs := []*net.IPNet{parseCIDR(t, "10.244.0.0/16")}
	got := RemoveLargerOverlappingCIDRs(cidrs)
	if len(got) != 1 || got[0].String() != "10.244.0.0/16" {
		t.Fatalf("expected single CIDR preserved, got %v", got)
	}
}

func TestRemoveCIDRsContainingIPs_IPOutsideAllCIDRs(t *testing.T) {
	cidrs := []*net.IPNet{
		parseCIDR(t, "10.0.0.0/24"),
		parseCIDR(t, "172.16.0.0/16"),
	}
	// IP that doesn't belong to any CIDR
	ips := []net.IP{net.ParseIP("192.168.1.1")}
	got := RemoveCIDRsContainingIPs(cidrs, ips)
	if len(got) != 2 {
		t.Fatalf("expected all CIDRs preserved when IP is outside, got %d", len(got))
	}
}

func TestRemoveCIDRsContainingIPs_IPInsideOneCIDR(t *testing.T) {
	cidrs := []*net.IPNet{
		parseCIDR(t, "10.0.0.0/24"),
		parseCIDR(t, "172.16.0.0/16"),
		parseCIDR(t, "192.168.0.0/16"),
	}
	// Only matches 172.16.0.0/16
	ips := []net.IP{net.ParseIP("172.16.5.10")}
	got := RemoveCIDRsContainingIPs(cidrs, ips)
	if len(got) != 2 {
		t.Fatalf("expected 2 CIDRs remaining, got %d: %v", len(got), got)
	}
	for _, c := range got {
		if c.String() == "172.16.0.0/16" {
			t.Fatalf("172.16.0.0/16 should have been removed")
		}
	}
}

func TestRemoveCIDRsContainingIPs_MultipleIPsRemoveMultipleCIDRs(t *testing.T) {
	cidrs := []*net.IPNet{
		parseCIDR(t, "10.0.0.0/8"),
		parseCIDR(t, "172.16.0.0/12"),
		parseCIDR(t, "192.168.0.0/16"),
	}
	ips := []net.IP{
		net.ParseIP("10.1.2.3"),
		net.ParseIP("192.168.1.1"),
	}
	got := RemoveCIDRsContainingIPs(cidrs, ips)
	if len(got) != 1 {
		t.Fatalf("expected 1 CIDR remaining, got %d: %v", len(got), got)
	}
	if got[0].String() != "172.16.0.0/12" {
		t.Fatalf("expected 172.16.0.0/12, got %s", got[0].String())
	}
}

func TestRemoveCIDRsContainingIPs_NilInputs(t *testing.T) {
	// nil cidrs
	got := RemoveCIDRsContainingIPs(nil, []net.IP{net.ParseIP("10.0.0.1")})
	if len(got) != 0 {
		t.Fatalf("expected empty for nil cidrs, got %v", got)
	}

	// nil ips - all CIDRs preserved
	cidrs := []*net.IPNet{parseCIDR(t, "10.0.0.0/8")}
	got = RemoveCIDRsContainingIPs(cidrs, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 CIDR preserved with nil IPs, got %d", len(got))
	}
}

func TestMergeToSupernet(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string // order-independent
	}{
		{
			name: "nearby v4 pair merges within floor",
			in:   []string{"10.96.0.0/24", "10.96.5.0/24"},
			want: []string{"10.96.0.0/21"},
		},
		{
			name: "scattered v4 reconstructs standard service CIDR",
			in:   []string{"10.96.0.0/24", "10.107.5.0/24"},
			want: []string{"10.96.0.0/12"},
		},
		{
			name: "far-apart v4 refuses to merge (below floor)",
			in:   []string{"10.96.0.0/24", "172.16.0.0/24"},
			want: []string{"10.96.0.0/24", "172.16.0.0/24"},
		},
		{
			name: "single member returned as-is",
			in:   []string{"10.96.0.0/24"},
			want: []string{"10.96.0.0/24"},
		},
		{
			name: "duplicate members deduplicated",
			in:   []string{"10.96.0.0/24", "10.96.0.0/24"},
			want: []string{"10.96.0.0/24"},
		},
		{
			name: "dual-stack: v4 merges, distant v6 kept separate",
			in:   []string{"10.96.0.0/24", "10.96.5.0/24", "fd00::/64", "fd01::/64"},
			want: []string{"10.96.0.0/21", "fd00::/64", "fd01::/64"},
		},
		{
			name: "nearby v6 pair merges within floor",
			in:   []string{"fd00:0:0:1::/64", "fd00:0:0:2::/64"},
			want: []string{"fd00::/62"},
		},
		{
			name: "far-apart v6 refuses to merge (below floor)",
			in:   []string{"fd00:0:1::/64", "fd00:0:2::/64"},
			want: []string{"fd00:0:1::/64", "fd00:0:2::/64"},
		},
		{
			name: "empty input",
			in:   nil,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in []*net.IPNet
			for _, s := range tt.in {
				in = append(in, parseCIDR(t, s))
			}
			got := cidrSet(mergeToSupernet(in))
			want := sets.New[string](tt.want...)
			if !got.Equal(want) {
				t.Errorf("mergeToSupernet(%v) = %v, want %v", tt.in, got.UnsortedList(), want.UnsortedList())
			}
		})
	}
}

// parseCIDR is a test helper that parses a CIDR string or fails the test.
func parseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("failed to parse CIDR %q: %v", s, err)
	}
	return ipNet
}

func TestBuildCIDRPodSpec(t *testing.T) {
	const (
		testNamespace = "test-ns"
		testImage     = "ghcr.io/kubenetworks/kubevpn:test"
	)
	pod := buildCIDRPodSpec(testNamespace, testImage)

	t.Run("pod name and namespace", func(t *testing.T) {
		if pod.Name != config.CniNetName {
			t.Errorf("pod name = %q, want %q", pod.Name, config.CniNetName)
		}
		if pod.Namespace != testNamespace {
			t.Errorf("pod namespace = %q, want %q", pod.Namespace, testNamespace)
		}
	})

	t.Run("volumes include CNI and proc host paths", func(t *testing.T) {
		if len(pod.Spec.Volumes) != 2 {
			t.Fatalf("expected 2 volumes, got %d", len(pod.Spec.Volumes))
		}

		cniVol := pod.Spec.Volumes[0]
		if cniVol.Name != config.CniNetName {
			t.Errorf("first volume name = %q, want %q", cniVol.Name, config.CniNetName)
		}
		if cniVol.HostPath == nil || cniVol.HostPath.Path != config.DefaultNetDir {
			t.Errorf("first volume host path = %v, want %q", cniVol.HostPath, config.DefaultNetDir)
		}

		procVol := pod.Spec.Volumes[1]
		if procVol.Name != "proc-dir-kubevpn" {
			t.Errorf("second volume name = %q, want %q", procVol.Name, "proc-dir-kubevpn")
		}
		if procVol.HostPath == nil || procVol.HostPath.Path != config.Proc {
			t.Errorf("second volume host path = %v, want %q", procVol.HostPath, config.Proc)
		}
	})

	t.Run("container image and command", func(t *testing.T) {
		if len(pod.Spec.Containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
		}
		c := pod.Spec.Containers[0]
		if c.Image != testImage {
			t.Errorf("container image = %q, want %q", c.Image, testImage)
		}
		wantCmd := []string{"tail", "-f", "/dev/null"}
		if !reflect.DeepEqual(c.Command, wantCmd) {
			t.Errorf("container command = %v, want %v", c.Command, wantCmd)
		}
	})

	t.Run("affinity prefers master and control-plane nodes", func(t *testing.T) {
		if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
			t.Fatal("expected node affinity to be set")
		}
		prefs := pod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		if len(prefs) != 1 {
			t.Fatalf("expected 1 preferred scheduling term, got %d", len(prefs))
		}
		exprs := prefs[0].Preference.MatchExpressions
		if len(exprs) != 2 {
			t.Fatalf("expected 2 match expressions, got %d", len(exprs))
		}
		wantKeys := map[string]bool{
			"node-role.kubernetes.io/master":        false,
			"node-role.kubernetes.io/control-plane": false,
		}
		for _, expr := range exprs {
			if _, ok := wantKeys[expr.Key]; !ok {
				t.Errorf("unexpected affinity key %q", expr.Key)
			} else {
				wantKeys[expr.Key] = true
			}
		}
		for k, found := range wantKeys {
			if !found {
				t.Errorf("missing affinity key %q", k)
			}
		}
	})

	t.Run("tolerations allow control-plane scheduling", func(t *testing.T) {
		if len(pod.Spec.Tolerations) != 2 {
			t.Fatalf("expected 2 tolerations, got %d", len(pod.Spec.Tolerations))
		}
		wantKeys := map[string]bool{
			"node-role.kubernetes.io/master":        false,
			"node-role.kubernetes.io/control-plane": false,
		}
		for _, tol := range pod.Spec.Tolerations {
			if _, ok := wantKeys[tol.Key]; !ok {
				t.Errorf("unexpected toleration key %q", tol.Key)
			} else {
				wantKeys[tol.Key] = true
			}
		}
		for k, found := range wantKeys {
			if !found {
				t.Errorf("missing toleration key %q", k)
			}
		}
	})

	t.Run("topology spread constraints are set", func(t *testing.T) {
		if len(pod.Spec.TopologySpreadConstraints) != 1 {
			t.Fatalf("expected 1 topology spread constraint, got %d", len(pod.Spec.TopologySpreadConstraints))
		}
		tsc := pod.Spec.TopologySpreadConstraints[0]
		if tsc.TopologyKey != "kubernetes.io/hostname" {
			t.Errorf("topology key = %q, want %q", tsc.TopologyKey, "kubernetes.io/hostname")
		}
		if tsc.MaxSkew != 1 {
			t.Errorf("max skew = %d, want 1", tsc.MaxSkew)
		}
	})
}
