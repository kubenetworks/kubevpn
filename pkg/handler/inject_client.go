package handler

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// managerDialTimeout bounds how long we wait to reach the traffic manager's
// TunConfigService over the VPN before failing. The server-side operation itself
// (rollout wait) can take much longer, so this bounds only connection setup.
const managerDialTimeout = 5 * time.Second

// dialManager dials the traffic manager's TunConfigService over the VPN (its Service
// ClusterIP:9002, reachable once connected). Injection is server-side only (no local
// fallback), so an unreachable manager is a hard error. Blocking dial bounds the wait.
func (c *ConnectOptions) dialManager(ctx context.Context) (*grpc.ClientConn, error) {
	svc, err := c.clientset.CoreV1().Services(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get traffic manager service: %w", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == v1.ClusterIPNone {
		return nil, fmt.Errorf("traffic manager service has no ClusterIP")
	}
	addr := net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(config.PortXDS)))
	dialCtx, cancel := context.WithTimeout(ctx, managerDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("dial traffic manager %s: %w", addr, err)
	}
	return conn, nil
}

// createRemoteInboundViaManager performs sidecar injection server-side: it dials the
// traffic manager's ProxyInject RPC over the VPN and streams progress. Injection runs
// entirely in the pod with the manager's own ServiceAccount; there is no local fallback,
// so any failure (unreachable manager, missing RBAC, workload error) is returned. On
// success it starts the client-side port Mapper for any K8s Service workloads the manager
// reports — the SSH reverse tunnels to the developer's machine cannot run in the pod.
func (c *ConnectOptions) createRemoteInboundViaManager(ctx context.Context, namespace string, headers map[string]string, portMap []string, image, localTunIPv4, localTunIPv6 string, workloads []string) error {
	conn, err := c.dialManager(ctx)
	if err != nil {
		return err
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
		return fmt.Errorf("server-side inject: %w", err)
	}

	var services []*rpc.ServiceWorkload
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("server-side inject: %w", recvErr)
		}
		if resp.GetMessage() != "" {
			plog.G(ctx).Info(resp.GetMessage())
		}
		if len(resp.GetServices()) > 0 {
			services = resp.GetServices()
		}
	}

	c.startServiceMappers(namespace, headers, portMap, services)
	return nil
}

// leaveViaManager unpatches workloads server-side via the manager's LeaveInject RPC over
// the VPN. An empty workloads slice means "leave all workloads this OwnerID proxies in
// the namespace" (the manager derives them from ENVOY_CONFIG) — used by disconnect
// cleanup. No local fallback: failures are returned.
func (c *ConnectOptions) leaveViaManager(ctx context.Context, namespace string, workloads []string, ownerID string) error {
	conn, err := c.dialManager(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := rpc.NewTunConfigServiceClient(conn).LeaveInject(ctx, &rpc.InjectRequest{
		Namespace: namespace,
		Workloads: workloads,
		OwnerID:   ownerID,
	})
	if err != nil {
		return fmt.Errorf("server-side leave: %w", err)
	}

	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("server-side leave: %w", recvErr)
		}
		if resp.GetMessage() != "" {
			plog.G(ctx).Info(resp.GetMessage())
		}
	}
	return nil
}

// startServiceMappers starts a client-side port Mapper for each K8s Service workload the
// manager injected. The Mapper opens SSH reverse tunnels from the sidecar back to the
// developer's local ports, so it must run on the client even though injection itself ran
// server-side.
func (c *ConnectOptions) startServiceMappers(namespace string, headers map[string]string, portMap []string, services []*rpc.ServiceWorkload) {
	if len(services) == 0 {
		return
	}
	if c.proxyManager == nil {
		c.proxyManager = newProxyManager()
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
// (used after a server-side leave). ProxyList.Remove stops the Mapper before dropping it;
// a no-op when no Mapper is tracked (e.g. mesh workloads).
func (c *ConnectOptions) stopServiceMappers(namespace string, workloads []string) {
	if c.proxyManager == nil {
		return
	}
	for _, w := range workloads {
		c.proxyManager.Remove(namespace, w)
	}
}
