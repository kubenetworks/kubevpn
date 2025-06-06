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
	StatusOk     = "connected"
	StatusFailed = "disconnected"

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
				status.ProxyList, status.CloneList, err = gen(ctx, svr.connect, svr.clone)
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
			status.ProxyList, status.CloneList, err = gen(ctx, svr.connect, svr.clone)
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
		Namespace:  connect.OriginNamespace,
		Status:     status,
		Netif:      tunName,
	}
	return &info
}

func gen(ctx context.Context, connect *handler.ConnectOptions, clone *handler.CloneOptions) ([]*rpc.Proxy, []*rpc.Clone, error) {
	var proxyList []*rpc.Proxy
	if connect != nil && connect.GetClientset() != nil {
		mapInterface := connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace)
		configMap, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
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
			virtual.Uid = util.ConvertUidToWorkload(virtual.Uid)
			var isFargateMode bool
			for _, port := range virtual.Ports {
				if port.EnvoyListenerPort != 0 {
					isFargateMode = true
				}
			}

			var proxyRule []*rpc.ProxyRule
			for _, rule := range virtual.Rules {
				proxyRule = append(proxyRule, &rpc.ProxyRule{
					Headers:      rule.Headers,
					LocalTunIPv4: rule.LocalTunIPv4,
					LocalTunIPv6: rule.LocalTunIPv6,
					CurrentDevice: util.If(isFargateMode,
						connect.IsMe(virtual.Namespace, util.ConvertWorkloadToUid(virtual.Uid), rule.Headers),
						v4 == rule.LocalTunIPv4 && v6 == rule.LocalTunIPv6,
					),
					PortMap: useSecondPort(rule.PortMap),
				})
			}
			proxyList = append(proxyList, &rpc.Proxy{
				ClusterID:  connect.GetClusterID(),
				Cluster:    util.GetKubeconfigCluster(connect.GetFactory()),
				Kubeconfig: connect.OriginKubeconfigPath,
				Namespace:  virtual.Namespace,
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
				namespace = connect.OriginNamespace
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
						Headers:     clone.Headers,
						DstWorkload: clone.TargetWorkloadNames[workload],
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
