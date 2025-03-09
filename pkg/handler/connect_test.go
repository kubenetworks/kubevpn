package handler

import (
	"net"
	"testing"

	"github.com/google/gopacket/routing"
	"github.com/libp2p/go-netroute"
)

func TestRoute(t *testing.T) {
	var r routing.Router
	var err error
	r, err = netroute.New()
	if err != nil {
		t.Fatal(err)
	}
	iface, gateway, src, err := r.Route(net.ParseIP("8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("iface: %s, gateway: %s, src: %s", iface.Name, gateway, src)
}

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

			c := &ConnectOptions{
				cidrs: cidrs,
			}
			c.removeCIDRsContainingIPs(ipList)
			if !test.expectPanic {
				if len(c.cidrs) != len(test.expectedCIDRs) {
					t.Fatalf("unexpected number of remaining CIDRs: got %d, want %d", len(c.cidrs), len(test.expectedCIDRs))
				}
				for i, cidr := range c.cidrs {
					if cidr.String() != test.expectedCIDRs[i] {
						t.Errorf("unexpected CIDR at index %d: got %s, want %s", i, cidr.String(), test.expectedCIDRs[i])
					}
				}
			}
		})
	}
}

func Test_newer(t *testing.T) {
	type args struct {
		clientVersionStr string
		clientImgStr     string
		serverImgStr     string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(docker.io/naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(docker.io/naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(docker.io/naison/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.3.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.3.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1 (not same as client version, --image=xxx)
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client cli version(v1.3.1) not same as client image(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.3.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.0.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(ghcr.io/kubenetworks/kubevpn:v1.0.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.0.1",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(ghcr.io/kubenetworks/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.3.1",
			},
			want:    false,
			wantErr: false,
		},

		// custom server image registry, but client image is not same as client version, does not upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, but client image is not same as client version, does not upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    false,
			wantErr: false,
		},

		// custom server image registry, client image is same as client version,  upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, client image is same as client version,  upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newer(tt.args.clientVersionStr, tt.args.clientImgStr, tt.args.serverImgStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("newer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("newer() got = %v, want %v", got, tt.want)
			}
		})
	}
}
