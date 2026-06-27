package action

import (
	"context"
	"net"
	"sync"

	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	StatusOk        = "connected"
	StatusFailed    = "disconnected"
	StatusUnhealthy = "unhealthy"
)

// Status handles the Status RPC, reporting connection and proxy state.
func (svr *Server) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	var ips map[string]tunIP
	if !svr.IsSudo {
		ips = svr.getSudoTunIPs(ctx)
	}

	if len(req.ConnectionIDs) != 0 {
		svr.connMu.RLock()
		var matched []*handler.ConnectOptions
		for _, connectionID := range req.ConnectionIDs {
			if opt, _ := svr.findConnection(connectionID); opt != nil {
				matched = append(matched, opt)
			}
		}
		currentID := svr.currentConnectionID
		svr.connMu.RUnlock()

		var list []*rpc.Status
		var lock = &sync.Mutex{}
		wg := sync.WaitGroup{}
		for _, options := range matched {
			wg.Add(1)
			go func(options *handler.ConnectOptions) {
				defer wg.Done()
				result := buildConnectionStatus(options, ips)
				var err error
				result.ProxyList, result.SyncList, err = buildProxyAndSyncStatus(options, options.Sync)
				if err != nil {
					plog.G(ctx).Errorf("Error generating status: %v", err)
					result.Status = StatusUnhealthy
				}
				lock.Lock()
				list = append(list, result)
				lock.Unlock()
			}(options)
		}
		wg.Wait()
		return &rpc.StatusResponse{List: list, CurrentConnectionID: currentID}, nil
	}

	svr.connMu.RLock()
	snapshot := make([]*handler.ConnectOptions, len(svr.connections))
	copy(snapshot, svr.connections)
	currentID := svr.currentConnectionID
	svr.connMu.RUnlock()

	wg := sync.WaitGroup{}
	wg.Add(len(snapshot))
	list := make([]*rpc.Status, len(snapshot))
	for i, options := range snapshot {
		go func(i int, options *handler.ConnectOptions) {
			defer wg.Done()
			result := buildConnectionStatus(options, ips)
			var err error
			result.ProxyList, result.SyncList, err = buildProxyAndSyncStatus(options, options.Sync)
			if err != nil {
				plog.G(ctx).Errorf("Error generating status: %v", err)
				result.Status = StatusUnhealthy
			}
			list[i] = result
		}(i, options)
	}
	wg.Wait()
	return &rpc.StatusResponse{List: list, CurrentConnectionID: currentID}, nil
}

func resolveTunIP(connect *handler.ConnectOptions, ips map[string]tunIP) (v4, v6 string) {
	if ips != nil {
		if ip, ok := ips[connect.GetConnectionID()]; ok {
			return ip.v4, ip.v6
		}
	}
	return connect.GetLocalTunIP()
}

func buildConnectionStatus(connect *handler.ConnectOptions, ips map[string]tunIP) *rpc.Status {
	v4, v6 := resolveTunIP(connect, ips)
	status := StatusOk
	tunName := ""
	var tunIPs []net.IP
	if v4 != "" {
		tunIPs = append(tunIPs, net.ParseIP(v4))
	}
	if v6 != "" {
		tunIPs = append(tunIPs, net.ParseIP(v6))
	}
	if len(tunIPs) > 0 {
		if dev, err := util.GetTunDevice(tunIPs...); err == nil {
			tunName = dev.Name
		}
	}
	if tunName == "" {
		status = StatusFailed
	}
	info := rpc.Status{
		ConnectionID: connect.GetConnectionID(),
		Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
		Kubeconfig:   connect.OriginKubeconfigPath,
		Namespace:    connect.WorkloadNamespace,
		Status:       status,
		Netif:        tunName,
		IPv4:         v4,
		IPv6:         v6,
	}
	return &info
}

func buildProxyAndSyncStatus(connect *handler.ConnectOptions, sync *handler.SyncOptions) ([]*rpc.Proxy, []*rpc.Sync, error) {
	var proxyList []*rpc.Proxy
	status := connect.HealthStatus()
	if configMap := status.ConfigMap(); configMap != nil {
		v := make([]*controlplane.Virtual, 0)
		if str, ok := configMap.Data[config.KeyEnvoy]; ok {
			if err := yaml.Unmarshal([]byte(str), &v); err != nil {
				return nil, nil, err
			}
		}
		for _, virtual := range v {
			// deployments.apps.ry-server --> deployments.apps/ry-server
			virtual.UID = util.ConvertUIDToWorkload(virtual.UID)
			var proxyRule []*rpc.ProxyRule
			for _, rule := range virtual.Rules {
				proxyRule = append(proxyRule, &rpc.ProxyRule{
					Headers:       rule.Headers,
					LocalTunIPv4:  rule.LocalTunIPv4,
					LocalTunIPv6:  rule.LocalTunIPv6,
					CurrentDevice: rule.OwnerID == connect.OwnerID,
					PortMap:       portMapToLocalPorts(rule),
				})
			}
			proxyList = append(proxyList, &rpc.Proxy{
				ConnectionID: connect.GetConnectionID(),
				Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
				Kubeconfig:   connect.OriginKubeconfigPath,
				Namespace:    virtual.Namespace,
				Workload:     virtual.UID,
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
				namespace = connect.WorkloadNamespace
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

	return proxyList, syncList, status.LastError()
}

// portMapToLocalPorts extracts containerPort → localPort mappings using the typed ParsePortMap helper.
func portMapToLocalPorts(rule *controlplane.Rule) map[int32]int32 {
	result := make(map[int32]int32)
	for _, pm := range rule.ParsePortMap() {
		result[pm.ContainerPort] = pm.LocalPort
	}
	return result
}
