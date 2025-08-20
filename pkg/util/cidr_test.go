package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type cidrUt struct {
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	restconfig *rest.Config
	f          util.Factory
}

func (u *cidrUt) init() {
	var err error
	configFlags := genericclioptions.NewConfigFlags(true)
	u.f = util.NewFactory(util.NewMatchVersionFlags(configFlags))

	if u.restconfig, err = u.f.ToRESTConfig(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.restclient, err = rest.RESTClientFor(u.restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.clientset, err = kubernetes.NewForConfig(u.restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.namespace, _, err = u.f.ToRawKubeConfigLoader().Namespace(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
}

func TestByDumpClusterInfo(t *testing.T) {
	u := &cidrUt{}
	u.init()
	info, err := GetCIDRByDumpClusterInfo(context.Background(), u.clientset)
	if err != nil {
		t.Log(err.Error())
	}
	for _, ipNet := range info {
		t.Log(ipNet.String())
	}
}

func TestByCreateSvc(t *testing.T) {
	u := &cidrUt{}
	u.init()
	info, err := GetServiceCIDRByCreateService(context.Background(), u.clientset.CoreV1().Services("default"))
	if err != nil {
		t.Log(err.Error())
	}
	if info != nil {
		t.Log(info.String())
	}
}

func TestElegant(t *testing.T) {
	u := &cidrUt{}
	u.init()
	elegant := GetCIDR(context.Background(), u.clientset, u.restconfig, u.namespace, config.Image)
	for _, ipNet := range elegant {
		t.Log(ipNet.String())
	}
}

func TestWaitBackoff(t *testing.T) {
	var last = time.Now()
	_ = retry.OnError(
		wait.Backoff{
			Steps:    10,
			Duration: time.Millisecond * 50,
		}, func(err error) bool {
			return err != nil
		}, func() error {
			now := time.Now()
			fmt.Println(now.Sub(last).String())
			last = now
			return fmt.Errorf("")
		})
}

func TestArray(t *testing.T) {
	s := []int{1, 2, 3, 1, 2, 3, 1, 2, 3}
	for i := 0; i < 3; i++ {
		ints := s[i*3 : i*3+3]
		println(ints[0], ints[1], ints[2])
	}
}

func TestPatch(t *testing.T) {
	var p = v1.Probe{
		ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{
			Path:   "/health",
			Port:   intstr.FromInt32(9080),
			Scheme: "HTTP",
		}},
	}
	marshal, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(marshal))

	var pp v1.Probe
	err = json.Unmarshal(marshal, &pp)
	if err != nil {
		panic(err)
	}
	bytes, err := yaml.Marshal(pp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(bytes))
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
