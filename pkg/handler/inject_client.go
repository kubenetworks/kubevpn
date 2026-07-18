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

const (
	// managerDialTimeout bounds a single attempt to reach the traffic manager's
	// TunConfigService over the VPN. The server-side operation itself (rollout wait)
	// can take much longer, so this bounds only connection setup.
	managerDialTimeout = 5 * time.Second

	// managerDialRetryBudget bounds the total time the inbound-injection dial retries
	// across transient failures. Inbound injection dials the manager over a VPN that
	// was just brought up, and its data path (k8s port-forward) can briefly wedge and
	// self-heal — network.go's watchLiveness forces a reconnect within
	// livenessStartupDeadline (30s). A single un-retried 5s dial that lands in that
	// window aborts the whole connect ("envoy sidecar injection failed"); retrying past
	// 30s turns the blip into a short delay instead. Leave stays single-shot (best
	// effort — the manager's lease reaper reclaims rules), so only inject retries.
	managerDialRetryBudget = 45 * time.Second

	// managerDialRetryBackoff is the pause between inbound-injection dial attempts.
	managerDialRetryBackoff = 2 * time.Second
)

// resolveManagerAddr looks up the traffic manager Service and returns its xDS dial
// address (ClusterIP:9002). A missing Service or a headless/empty ClusterIP is a
// permanent configuration error, NOT a transient one — callers must fail fast on it
// rather than retry (injection is server-side only, with no local fallback).
func (c *ConnectOptions) resolveManagerAddr(ctx context.Context) (string, error) {
	// Bound only this apiserver GET: without a deadline it inherits client-go's ~30s dial
	// timeout, so a teardown against an unreachable cluster stalls ~30s here. The dial and
	// gRPC stream that follow use the caller's ctx and are unaffected.
	getCtx, cancel := context.WithTimeout(ctx, config.ManagerServiceGetTimeout)
	defer cancel()
	svc, err := c.clientset.CoreV1().Services(c.ManagerNamespace).Get(getCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get traffic manager service: %w", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == v1.ClusterIPNone {
		return "", fmt.Errorf("traffic manager service has no ClusterIP")
	}
	return net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(config.PortXDS))), nil
}

// dialManager dials the traffic manager's TunConfigService over the VPN (its Service
// ClusterIP:9002, reachable once connected). Single blocking attempt bounded by
// managerDialTimeout — used by the best-effort leave path (the manager's lease reaper
// reclaims rules if it fails, so leave does not retry).
func (c *ConnectOptions) dialManager(ctx context.Context) (*grpc.ClientConn, error) {
	addr, err := c.resolveManagerAddr(ctx)
	if err != nil {
		return nil, err
	}
	return c.dialManagerAddr(ctx, addr)
}

// dialManagerAddr performs one blocking gRPC dial to addr, bounded by managerDialTimeout.
func (c *ConnectOptions) dialManagerAddr(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, managerDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, fmt.Errorf("dial traffic manager %s: %w", addr, err)
	}
	return conn, nil
}

// dialManagerWithRetry resolves the manager address once (permanent errors fail fast),
// then retries only the blocking dial with a fixed backoff up to managerDialRetryBudget.
// This absorbs a transient port-forward reconnect right after the VPN comes up, so
// inbound injection does not abort the entire connect on a single timed-out dial. It
// stops early if ctx is cancelled; the last dial error is returned.
func (c *ConnectOptions) dialManagerWithRetry(ctx context.Context) (*grpc.ClientConn, error) {
	addr, err := c.resolveManagerAddr(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(managerDialRetryBudget)
	for attempt := 1; ; attempt++ {
		conn, err := c.dialManagerAddr(ctx, addr)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil || !time.Now().Add(managerDialRetryBackoff).Before(deadline) {
			return nil, err
		}
		plog.G(ctx).Debugf("dial traffic manager %s failed (attempt %d), retrying in %s: %v", addr, attempt, managerDialRetryBackoff, err)
		select {
		case <-time.After(managerDialRetryBackoff):
		case <-ctx.Done():
			return nil, err
		}
	}
}

// createRemoteInboundViaManager performs sidecar injection server-side: it dials the
// traffic manager's ProxyInject RPC over the VPN and streams progress. Injection runs
// entirely in the pod with the manager's own ServiceAccount; there is no local fallback,
// so any failure (unreachable manager, missing RBAC, workload error) is returned. On
// success it starts the client-side port Mapper for any K8s Service workloads the manager
// reports — the SSH reverse tunnels to the developer's machine cannot run in the pod.
func (c *ConnectOptions) createRemoteInboundViaManager(ctx context.Context, namespace string, headers map[string]string, portMap []string, image, localTunIPv4, localTunIPv6 string, workloads []string) error {
	conn, err := c.dialManagerWithRetry(ctx)
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
