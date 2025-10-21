package action

import (
	"context"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	StatusOk     = "connected"
	StatusFailed = "disconnected"
)

func (svr *Server) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var list []*rpc.Status

	timeoutCtx, cancelFunc := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFunc()
	if len(req.ConnectionIDs) != 0 {
		for _, connectionID := range req.ConnectionIDs {
			for _, options := range svr.connections {
				if options.GetConnectionID() == connectionID {
					result := genStatus(options)
					var err error
					result.ProxyList, result.SyncList, err = gen(timeoutCtx, options, options.Sync)
					if err != nil {
						plog.G(context.Background()).Errorf("Error generating status: %v", err)
					}
					list = append(list, result)
				}
			}
		}
	} else {
		for _, options := range svr.connections {
			result := genStatus(options)
			var err error
			result.ProxyList, result.SyncList, err = gen(timeoutCtx, options, options.Sync)
			if err != nil {
				plog.G(context.Background()).Errorf("Error generating status: %v", err)
			}
			list = append(list, result)
		}
	}
	return &rpc.StatusResponse{List: list, CurrentConnectionID: svr.currentConnectionID}, nil
}

func genStatus(connect *handler.ConnectOptions) *rpc.Status {
	status := StatusOk
	tunName, _ := connect.GetTunDeviceName()
	if tunName == "" {
		status = StatusFailed
	}
	info := rpc.Status{
		ConnectionID: connect.GetConnectionID(),
		Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
		Kubeconfig:   connect.OriginKubeconfigPath,
		Namespace:    connect.OriginNamespace,
		Status:       status,
		Netif:        tunName,
	}
	return &info
}

func gen(ctx context.Context, connect *handler.ConnectOptions, sync *handler.SyncOptions) ([]*rpc.Proxy, []*rpc.Sync, error) {
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
				ConnectionID: connect.GetConnectionID(),
				Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
				Kubeconfig:   connect.OriginKubeconfigPath,
				Namespace:    virtual.Namespace,
				Workload:     virtual.Uid,
				RuleList:     proxyRule,
			})
		}
	}
	var syncList []*rpc.Sync
	if sync != nil {
		for _, workload := range sync.Workloads {
			var connectionID, cluster, kubeconfig, namespace string
			if connect != nil {
				connectionID = connect.GetConnectionID()
				cluster = util.GetKubeconfigCluster(connect.GetFactory())
				kubeconfig = connect.OriginKubeconfigPath
				namespace = connect.OriginNamespace
			}
			syncList = append(syncList, &rpc.Sync{
				ConnectionID:     connectionID,
				Cluster:          cluster,
				Kubeconfig:       kubeconfig,
				Namespace:        namespace,
				Workload:         workload,
				SyncthingGUIAddr: sync.GetSyncthingGUIAddr(),
				RuleList: []*rpc.SyncRule{
					{
						Headers:     sync.Headers,
						DstWorkload: sync.TargetWorkloadNames[workload],
					},
				},
			})
		}
	}
	return proxyList, syncList, nil
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
