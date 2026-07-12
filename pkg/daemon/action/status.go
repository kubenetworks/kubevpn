package action

import (
	"context"
	"net"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
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
		ips = svr.sudoHealthSnapshot(ctx)
	}

	if len(req.ConnectionIDs) != 0 {
		svr.connMu.RLock()
		var matched []handler.Connection
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
			go func(options handler.Connection) {
				defer wg.Done()
				result := buildConnectionStatus(options, ips)
				var err error
				result.ProxyList, result.SyncList, err = buildProxyAndSyncStatus(ctx, options, options.GetSync())
				if err != nil {
					// Proxy/sync rendering failure is a ConfigMap-read issue, not a data-plane
					// liveness signal — log it but leave the (heartbeat-based) status intact.
					plog.G(ctx).Errorf("Error generating proxy/sync status: %v", err)
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
	snapshot := make([]handler.Connection, len(svr.connections))
	copy(snapshot, svr.connections)
	currentID := svr.currentConnectionID
	svr.connMu.RUnlock()

	wg := sync.WaitGroup{}
	wg.Add(len(snapshot))
	list := make([]*rpc.Status, len(snapshot))
	for i, options := range snapshot {
		go func(i int, options handler.Connection) {
			defer wg.Done()
			result := buildConnectionStatus(options, ips)
			var err error
			result.ProxyList, result.SyncList, err = buildProxyAndSyncStatus(ctx, options, options.GetSync())
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

func resolveTunIP(connect handler.Connection, ips map[string]tunIP) (v4, v6 string) {
	if ips != nil {
		if ip, ok := ips[connect.GetConnectionID()]; ok {
			return ip.v4, ip.v6
		}
	}
	return connect.GetLocalTunIP()
}

// heartbeatStaleThreshold is how long without an echo reply before the data plane is
// considered unhealthy: 1.5 × the heartbeat interval tolerates one missed beat's jitter.
var heartbeatStaleThreshold = config.KeepAliveTime * 3 / 2

// deriveConnectionStatus maps TUN presence and the local data-plane heartbeat into the reported
// status. It is computed in the sudo daemon, which owns the TUN and observes the heartbeat replies:
//
//	no TUN device                        -> disconnected
//	TUN up but heartbeat stale / never   -> unhealthy
//	TUN up, heartbeat fresh              -> connected
//
// The heartbeat echo reply is the end-to-end data-plane liveness signal (reachability to the server
// gateway); it also drives the port-forward black-hole watchdog, so a dead tunnel goes stale here.
func deriveConnectionStatus(tunUp bool, lastHeartbeat time.Time) string {
	if !tunUp {
		return StatusFailed
	}
	if lastHeartbeat.IsZero() || time.Since(lastHeartbeat) > heartbeatStaleThreshold {
		return StatusUnhealthy
	}
	return StatusOk
}

// resolveStatus returns the data-plane status verdict. The user daemon reuses the verdict the
// sudo daemon already computed (carried in the Status string via getSudoTunIPs); the sudo daemon
// computes it locally from its TUN + heartbeat. Mirrors resolveTunIP's user/sudo split.
func resolveStatus(connect handler.Connection, ips map[string]tunIP, tunUp bool) string {
	var connectionID string
	if connect != nil {
		connectionID = connect.GetConnectionID()
	}
	if ips != nil {
		if ip, ok := ips[connectionID]; ok {
			return ip.status
		}
		// User daemon, but the sudo daemon has no such connection: data plane is down.
		return StatusFailed
	}
	if connect == nil {
		return deriveConnectionStatus(tunUp, time.Time{})
	}
	return deriveConnectionStatus(tunUp, connect.GetLastHeartbeat())
}

func buildConnectionStatus(connect handler.Connection, ips map[string]tunIP) *rpc.Status {
	v4, v6 := resolveTunIP(connect, ips)
	tunName := ""
	var tunIPs []net.IP
	if v4 != "" {
		tunIPs = append(tunIPs, net.ParseIP(v4))
	}
	if v6 != "" {
		tunIPs = append(tunIPs, net.ParseIP(v6))
	}
	if len(tunIPs) > 0 {
		if dev, err := netutil.GetTunDevice(tunIPs...); err == nil {
			tunName = dev.Name
		}
	}

	info := rpc.Status{
		ConnectionID: connect.GetConnectionID(),
		Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
		Kubeconfig:   connect.GetOriginKubeconfigPath(),
		Namespace:    connect.GetWorkloadNamespace(),
		Status:       resolveStatus(connect, ips, tunName != ""),
		Netif:        tunName,
		IPv4:         v4,
		IPv6:         v6,
	}
	return &info
}

func buildProxyAndSyncStatus(ctx context.Context, connect handler.Connection, sync *handler.SyncOptions) ([]*rpc.Proxy, []*rpc.Sync, error) {
	var proxyList []*rpc.Proxy
	// Read the ConfigMap straight from the informer cache (near-real-time, zero API cost).
	configMap, cmErr := connect.GetTrafficManagerConfigMap(ctx)
	if cmErr != nil {
		return nil, nil, cmErr
	}
	if configMap != nil {
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
					CurrentDevice: rule.OwnerID == connect.GetOwnerID(),
					PortMap:       portMapToLocalPorts(rule),
				})
			}
			proxyList = append(proxyList, &rpc.Proxy{
				ConnectionID: connect.GetConnectionID(),
				Cluster:      util.GetKubeconfigCluster(connect.GetFactory()),
				Kubeconfig:   connect.GetOriginKubeconfigPath(),
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
				kubeconfig = connect.GetOriginKubeconfigPath()
				namespace = connect.GetWorkloadNamespace()
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

// portMapToLocalPorts extracts containerPort → localPort mappings using the typed ParsePortMap helper.
func portMapToLocalPorts(rule *controlplane.Rule) map[int32]int32 {
	result := make(map[int32]int32)
	for _, pm := range rule.ParsePortMap() {
		result[pm.ContainerPort] = pm.LocalPort
	}
	return result
}
