package cmds

import (
	"fmt"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestPrintProxyAndClone(t *testing.T) {
	var status = &rpc.StatusResponse{
		List: []*rpc.Status{
			{
				ConnectionID: "9c775fd22b84",
				Cluster:      "ccm6epn7qvcplhs3o8p00",
				Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun4",
				ProxyList: []*rpc.Proxy{
					{
						ConnectionID: "9c775fd22b84",
						Cluster:      "ccm6epn7qvcplhs3o8p00",
						Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:    "vke-system",
						Workload:     "deployment.apps/authors",
						RuleList: []*rpc.ProxyRule{
							{
								Headers:       map[string]string{"user": "naison"},
								LocalTunIPv4:  "198.19.0.103",
								LocalTunIPv6:  "2001:2::999d",
								CurrentDevice: false,
								PortMap:       map[int32]int32{8910: 8910},
							},
						},
					},
				},
				SyncList: []*rpc.Sync{
					{
						ConnectionID: "9c775fd22b84",
						Cluster:      "ccm6epn7qvcplhs3o8p00",
						Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:    "vke-system",
						Workload:     "deployment.apps/ratings",
						RuleList: []*rpc.SyncRule{{
							Headers:     map[string]string{"user": "naison"},
							DstWorkload: "deployment.apps/ratings-clone-5ngn6",
						}},
					},
				},
			},
			{
				ConnectionID: "38e6a2f11443",
				Cluster:      "ccnepblsebp68ivej4a20",
				Kubeconfig:   "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun5",
				ProxyList:    []*rpc.Proxy{},
				SyncList:     []*rpc.Sync{},
			},
		},
	}
	output, err := genOutput(status, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(output)
}

func TestPrintProxy(t *testing.T) {
	var status = &rpc.StatusResponse{
		List: []*rpc.Status{
			{
				ConnectionID: "9c775fd22b84",
				Cluster:      "ccm6epn7qvcplhs3o8p00",
				Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun4",
				ProxyList: []*rpc.Proxy{
					{
						ConnectionID: "9c775fd22b84",
						Cluster:      "ccm6epn7qvcplhs3o8p00",
						Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:    "vke-system",
						Workload:     "deployment.apps/authors",
						RuleList: []*rpc.ProxyRule{
							{
								Headers:       map[string]string{"user": "naison"},
								LocalTunIPv4:  "198.19.0.103",
								LocalTunIPv6:  "2001:2::999d",
								CurrentDevice: false,
								PortMap:       map[int32]int32{8910: 8910},
							},
						},
					},
				},
				SyncList: []*rpc.Sync{},
			},
			{
				ConnectionID: "38e6a2f11443",
				Cluster:      "ccnepblsebp68ivej4a20",
				Kubeconfig:   "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun5",
				ProxyList:    []*rpc.Proxy{},
				SyncList:     []*rpc.Sync{},
			},
		},
	}
	output, err := genOutput(status, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(output)
}

func TestPrintClone(t *testing.T) {
	var status = &rpc.StatusResponse{
		List: []*rpc.Status{
			{
				ConnectionID: "9c775fd22b84",
				Cluster:      "ccm6epn7qvcplhs3o8p00",
				Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun4",
				ProxyList:    []*rpc.Proxy{},
				SyncList: []*rpc.Sync{
					{
						ConnectionID: "9c775fd22b84",
						Cluster:      "ccm6epn7qvcplhs3o8p00",
						Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:    "vke-system",
						Workload:     "deployment.apps/ratings",
						RuleList: []*rpc.SyncRule{{
							Headers:     map[string]string{"user": "naison"},
							DstWorkload: "deployment.apps/ratings-clone-5ngn6",
						}},
					},
				},
			},
			{
				ConnectionID: "38e6a2f11443",
				Cluster:      "ccnepblsebp68ivej4a20",
				Kubeconfig:   "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun5",
				ProxyList:    []*rpc.Proxy{},
				SyncList:     []*rpc.Sync{},
			},
		},
	}
	output, err := genOutput(status, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(output)
}

func TestPrint(t *testing.T) {
	var status = &rpc.StatusResponse{
		List: []*rpc.Status{
			{
				ConnectionID: "9c775fd22b84",
				Cluster:      "ccm6epn7qvcplhs3o8p00",
				Kubeconfig:   "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun4",
				ProxyList:    []*rpc.Proxy{},
				SyncList:     []*rpc.Sync{},
			},
			{
				ConnectionID: "38e6a2f11443",
				Cluster:      "ccnepblsebp68ivej4a20",
				Kubeconfig:   "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:    "vke-system",
				Status:       "connected",
				Netif:        "utun5",
				ProxyList:    []*rpc.Proxy{},
				SyncList:     []*rpc.Sync{},
			},
		},
	}
	output, err := genOutput(status, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(output)
}
