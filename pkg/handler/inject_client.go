package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// managerDialTimeout bounds how long we wait to reach the traffic manager's
// TunConfigService over the VPN before giving up and using the local path. The
// server-side operation itself (rollout wait) can take much longer, so this bounds
// only connection setup.
const managerDialTimeout = 5 * time.Second

// dialManager dials the traffic manager's TunConfigService over the VPN (its Service
// ClusterIP:9002, reachable once connected). It returns (nil, nil) — deliberately not
// an error — when the manager has no usable ClusterIP or cannot be reached within
// managerDialTimeout, signalling the caller to fall back to the local path. Blocking
// dial ensures an unready VPN route degrades fast instead of hanging.
func (c *ConnectOptions) dialManager(ctx context.Context) (*grpc.ClientConn, error) {
	svc, err := c.clientset.CoreV1().Services(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil || svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == v1.ClusterIPNone {
		plog.G(ctx).Debugf("Server-side path unavailable (manager ClusterIP not found: %v); using local path", err)
		return nil, nil
	}
	addr := net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(config.PortXDS)))
	dialCtx, cancel := context.WithTimeout(ctx, managerDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		plog.G(ctx).Debugf("Server-side path unavailable (dial %s: %v); using local path", addr, err)
		return nil, nil
	}
	return conn, nil
}

// createRemoteInboundViaManager attempts server-side sidecar injection by dialing the
// traffic manager's ProxyInject RPC over the VPN. It returns:
//
//	handled=false, err=nil  -> the manager is unreachable / lacks the RPC or RBAC;
//	                           the caller must fall back to local injection.
//	handled=true,  err=nil  -> the manager performed the injection successfully.
//	handled=true,  err!=nil -> the manager attempted injection and failed definitively;
//	                           the caller must NOT fall back (would risk double injection).
//
// On success it starts the client-side port Mapper for any K8s Service workloads the
// manager reports — the SSH reverse tunnels to the developer's machine cannot run in
// the pod, so that side effect stays local.
func (c *ConnectOptions) createRemoteInboundViaManager(ctx context.Context, namespace string, headers map[string]string, portMap []string, image, localTunIPv4, localTunIPv6 string, workloads []string) (handled bool, err error) {
	conn, _ := c.dialManager(ctx)
	if conn == nil {
		return false, nil
	}
	defer conn.Close()

	stream, err := rpc.NewTunConfigServiceClient(conn).ProxyInject(ctx, &rpc.InjectRequest{
		Namespace:    namespace,
		Workloads:    workloads,
		Headers:      headers,
		PortMap:      portMap,
		OwnerID:      c.OwnerID,
		LocalTunIPv4: localTunIPv4,
		LocalTunIPv6: localTunIPv6,
		Image:        image,
	})
	if err != nil {
		plog.G(ctx).Debugf("Server-side inject unavailable (%v); using local injection", err)
		return false, nil
	}

	var services []*rpc.ServiceWorkload
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			// Unavailable/Unimplemented => older manager or no RBAC: fall back locally.
			if code := status.Code(recvErr); code == codes.Unavailable || code == codes.Unimplemented {
				plog.G(ctx).Debugf("Server-side inject not supported (%v); using local injection", recvErr)
				return false, nil
			}
			// The manager tried and failed: surface the error, do NOT double-inject locally.
			return true, fmt.Errorf("server-side inject: %w", recvErr)
		}
		if resp.GetMessage() != "" {
			plog.G(ctx).Info(resp.GetMessage())
		}
		if len(resp.GetServices()) > 0 {
			services = resp.GetServices()
		}
	}

	// The manager did the cluster writes; run the local Mapper for Service workloads.
	c.startServiceMappers(namespace, headers, portMap, services)
	return true, nil
}

// leaveViaManager attempts server-side unpatching of the given workloads (by OwnerID)
// via the manager's LeaveInject RPC over the VPN. The handled/err semantics match
// createRemoteInboundViaManager: handled=false means "fall back to local unpatch".
func (c *ConnectOptions) leaveViaManager(ctx context.Context, namespace string, workloads []string, ownerID string) (handled bool, err error) {
	conn, _ := c.dialManager(ctx)
	if conn == nil {
		return false, nil
	}
	defer conn.Close()

	stream, err := rpc.NewTunConfigServiceClient(conn).LeaveInject(ctx, &rpc.InjectRequest{
		Namespace: namespace,
		Workloads: workloads,
		OwnerID:   ownerID,
	})
	if err != nil {
		plog.G(ctx).Debugf("Server-side leave unavailable (%v); using local unpatch", err)
		return false, nil
	}

	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if code := status.Code(recvErr); code == codes.Unavailable || code == codes.Unimplemented {
				plog.G(ctx).Debugf("Server-side leave not supported (%v); using local unpatch", recvErr)
				return false, nil
			}
			return true, fmt.Errorf("server-side leave: %w", recvErr)
		}
		if resp.GetMessage() != "" {
			plog.G(ctx).Info(resp.GetMessage())
		}
	}
	return true, nil
}

// startServiceMappers starts a client-side port Mapper for each K8s Service workload
// the manager injected. The Mapper opens SSH reverse tunnels from the sidecar back to
// the developer's local ports, so it must run on the client even when injection itself
// ran server-side.
func (c *ConnectOptions) startServiceMappers(namespace string, headers map[string]string, portMap []string, services []*rpc.ServiceWorkload) {
	if len(services) == 0 {
		return
	}
	if c.proxyManager == nil {
		c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	}
	for _, s := range services {
		mapper := NewMapper(c.clientset, namespace, s.GetSelector(), headers, s.GetWorkload(), c.GetConfigMapInformer())
		c.proxyManager.Add(&Proxy{
			headers:    headers,
			portMap:    portMap,
			workload:   s.GetWorkload(),
			namespace:  namespace,
			portMapper: mapper,
		})
		go mapper.Run()
	}
}

// stopServiceMappers stops and removes any local port Mapper for the given workloads
// (used after a server-side leave). ProxyList.Remove stops the Mapper before dropping
// it; a no-op when no Mapper is tracked (e.g. mesh workloads).
func (c *ConnectOptions) stopServiceMappers(namespace string, workloads []string) {
	if c.proxyManager == nil {
		return
	}
	for _, w := range workloads {
		c.proxyManager.Remove(namespace, w)
	}
}
