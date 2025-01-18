package action

import (
	"context"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	StatusOk     = "Connected"
	StatusFailed = "Disconnected"

	ModeFull = "full"
	ModeLite = "lite"
)

func (svr *Server) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var list []*rpc.Status

	if len(req.ClusterIDs) != 0 {
		for _, clusterID := range req.ClusterIDs {
			if svr.connect.GetClusterID() == clusterID {
				status := genStatus(svr.connect, ModeFull, 0)
				var err error
				status.ProxyList, status.CloneList, err = gen(svr.connect, svr.clone)
				if err != nil {
					return nil, err
				}
				list = append(list, status)
			}
			for i, options := range svr.secondaryConnect {
				if options.GetClusterID() == clusterID {
					list = append(list, genStatus(options, ModeLite, int32(i+1)))
				}
			}
		}
	} else {
		if svr.connect != nil {
			status := genStatus(svr.connect, ModeFull, 0)
			var err error
			status.ProxyList, status.CloneList, err = gen(svr.connect, svr.clone)
			if err != nil {
				return nil, err
			}
			list = append(list, status)
		}

		for i, options := range svr.secondaryConnect {
			list = append(list, genStatus(options, ModeLite, int32(i+1)))
		}
	}
	return &rpc.StatusResponse{List: list}, nil
}

func genStatus(connect *handler.ConnectOptions, mode string, index int32) *rpc.Status {
	status := StatusOk
	tunName, _ := connect.GetTunDeviceName()
	if tunName == "" {
		status = StatusFailed
	}
	info := rpc.Status{
		ID:         index,
		ClusterID:  connect.GetClusterID(),
		Cluster:    util.GetKubeconfigCluster(connect.GetFactory()),
		Mode:       mode,
		Kubeconfig: connect.OriginKubeconfigPath,
		Namespace:  connect.Namespace,
		Status:     status,
		Netif:      tunName,
	}
	return &info
}

func gen(connect *handler.ConnectOptions, clone *handler.CloneOptions) ([]*rpc.Proxy, []*rpc.Clone, error) {
	var proxyList []*rpc.Proxy
	if connect != nil && connect.GetClientset() != nil {
		mapInterface := connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace)
		configMap, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		var v = make([]*controlplane.Virtual, 0)
		if str, ok := configMap.Data[config.KeyEnvoy]; ok {
			if err = yaml.Unmarshal([]byte(str), &v); err != nil {
				return nil, nil, err
			}
		}
		v4, v6 := connect.GetLocalTunIP()
		for _, virtual := range v {
			// deployments.apps.ry-server --> deployments.apps/ry-server
			lastIndex := strings.LastIndex(virtual.Uid, ".")
			virtual.Uid = virtual.Uid[:lastIndex] + "/" + virtual.Uid[lastIndex+1:]

			var proxyRule []*rpc.ProxyRule
			for _, rule := range virtual.Rules {
				proxyRule = append(proxyRule, &rpc.ProxyRule{
					Headers:       rule.Headers,
					LocalTunIPv4:  rule.LocalTunIPv4,
					LocalTunIPv6:  rule.LocalTunIPv6,
					CurrentDevice: v4 == rule.LocalTunIPv4 && v6 == rule.LocalTunIPv6,
					PortMap:       useSecondPort(rule.PortMap),
				})
			}
			proxyList = append(proxyList, &rpc.Proxy{
				ClusterID:  connect.GetClusterID(),
				Cluster:    util.GetKubeconfigCluster(connect.GetFactory()),
				Kubeconfig: connect.OriginKubeconfigPath,
				Namespace:  connect.Namespace,
				Workload:   virtual.Uid,
				RuleList:   proxyRule,
			})
		}
	}
	var cloneList []*rpc.Clone
	if clone != nil {
		for _, workload := range clone.Workloads {
			var clusterID, cluster, kubeconfig, namespace string
			if connect != nil {
				clusterID = connect.GetClusterID()
				cluster = util.GetKubeconfigCluster(connect.GetFactory())
				kubeconfig = connect.OriginKubeconfigPath
				namespace = connect.Namespace
			}
			cloneList = append(cloneList, &rpc.Clone{
				ClusterID:        clusterID,
				Cluster:          cluster,
				Kubeconfig:       kubeconfig,
				Namespace:        namespace,
				Workload:         workload,
				SyncthingGUIAddr: clone.GetSyncthingGUIAddr(),
				RuleList: []*rpc.CloneRule{
					{
						DstCluster:    util.GetKubeconfigCluster(clone.GetFactory()),
						Headers:       clone.Headers,
						DstWorkload:   clone.TargetWorkloadNames[workload],
						DstKubeconfig: clone.TargetKubeconfig,
						DstNamespace:  clone.TargetNamespace,
					},
				},
			})
		}
	}
	return proxyList, cloneList, nil
}

func useSecondPort(m map[int32]string) map[int32]int32 {
	var result = make(map[int32]int32)
	for k, v := range m {
		if strings.Index(v, ":") > 0 {
			v = strings.Split(v, ":")[1]
		}
		port, _ := strconv.Atoi(v)
		result[k] = int32(port)
	}
	return result
}
