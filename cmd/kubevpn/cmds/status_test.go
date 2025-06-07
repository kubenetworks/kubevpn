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
				ID:         0,
				ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
				Cluster:    "ccm6epn7qvcplhs3o8p00",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun4",
				ProxyList: []*rpc.Proxy{
					{
						ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
						Cluster:    "ccm6epn7qvcplhs3o8p00",
						Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:  "vke-system",
						Workload:   "deployment.apps/authors",
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
				CloneList: []*rpc.Clone{
					{
						ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
						Cluster:    "ccm6epn7qvcplhs3o8p00",
						Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:  "vke-system",
						Workload:   "deployment.apps/ratings",
						RuleList: []*rpc.CloneRule{{
							Headers:       map[string]string{"user": "naison"},
							DstClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
							DstCluster:    "ccm6epn7qvcplhs3o8p00",
							DstKubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
							DstNamespace:  "vke-system",
							DstWorkload:   "deployment.apps/ratings-clone-5ngn6",
						}},
					},
				},
			},
			{
				ID:         1,
				ClusterID:  "c08cae70-0021-46c9-a1dc-38e6a2f11443",
				Cluster:    "ccnepblsebp68ivej4a20",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun5",
				ProxyList:  []*rpc.Proxy{},
				CloneList:  []*rpc.Clone{},
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
				ID:         0,
				ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
				Cluster:    "ccm6epn7qvcplhs3o8p00",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun4",
				ProxyList: []*rpc.Proxy{
					{
						ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
						Cluster:    "ccm6epn7qvcplhs3o8p00",
						Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:  "vke-system",
						Workload:   "deployment.apps/authors",
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
				CloneList: []*rpc.Clone{},
			},
			{
				ID:         1,
				ClusterID:  "c08cae70-0021-46c9-a1dc-38e6a2f11443",
				Cluster:    "ccnepblsebp68ivej4a20",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun5",
				ProxyList:  []*rpc.Proxy{},
				CloneList:  []*rpc.Clone{},
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
				ID:         0,
				ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
				Cluster:    "ccm6epn7qvcplhs3o8p00",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun4",
				ProxyList:  []*rpc.Proxy{},
				CloneList: []*rpc.Clone{
					{
						ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
						Cluster:    "ccm6epn7qvcplhs3o8p00",
						Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
						Namespace:  "vke-system",
						Workload:   "deployment.apps/ratings",
						RuleList: []*rpc.CloneRule{{
							Headers:       map[string]string{"user": "naison"},
							DstClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
							DstCluster:    "ccm6epn7qvcplhs3o8p00",
							DstKubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
							DstNamespace:  "vke-system",
							DstWorkload:   "deployment.apps/ratings-clone-5ngn6",
						}},
					},
				},
			},
			{
				ID:         1,
				ClusterID:  "c08cae70-0021-46c9-a1dc-38e6a2f11443",
				Cluster:    "ccnepblsebp68ivej4a20",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun5",
				ProxyList:  []*rpc.Proxy{},
				CloneList:  []*rpc.Clone{},
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
				ID:         0,
				ClusterID:  "ac6d8dfb-1d23-4f2a-b11e-9c775fd22b84",
				Cluster:    "ccm6epn7qvcplhs3o8p00",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/test-feiyan-config-private-new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun4",
				ProxyList:  []*rpc.Proxy{},
				CloneList:  []*rpc.Clone{},
			},
			{
				ID:         1,
				ClusterID:  "c08cae70-0021-46c9-a1dc-38e6a2f11443",
				Cluster:    "ccnepblsebp68ivej4a20",
				Mode:       "full",
				Kubeconfig: "/Users/bytedance/.kube/dev_fy_config_new",
				Namespace:  "vke-system",
				Status:     "connected",
				Netif:      "utun5",
				ProxyList:  []*rpc.Proxy{},
				CloneList:  []*rpc.Clone{},
			},
		},
	}
	output, err := genOutput(status, FormatTable)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(output)
}
