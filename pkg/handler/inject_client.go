package handler

import (
	"context"
	"errors"
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
	// 30s turns the blip into a short delay instead. Leave uses a shorter retry budget
	// since it is a user-interactive, best-effort operation.
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

// createRemoteInboundViaManager publishes node-agent attachments through the
// manager's ProxyInject RPC. There is no local sidecar fallback, so manager,
// RBAC, workload, and readiness failures are returned to the caller.
func (c *ConnectOptions) createRemoteInboundViaManager(ctx context.Context, namespace string, headers map[string]string, portMap []string, image, localTunIPv4, localTunIPv6 string, workloads []string) error {
	if c.OwnerID == "" {
		return fmt.Errorf("cannot resolve owner ID for connection in namespace %s - reconnect and retry", namespace)
	}

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
	}
	return nil
}

// CreateRemoteSyncRoutes publishes original-to-clone routes through the existing
// manager connection. Sync targets do not use a user TUN IP or Service Mapper.
func (c *ConnectOptions) CreateRemoteSyncRoutes(ctx context.Context, namespace string, routes []SyncRoute, image string) error {
	if len(routes) == 0 {
		return nil
	}
	if c.OwnerID == "" {
		return fmt.Errorf("cannot resolve owner ID for connection in namespace %s - reconnect and retry", namespace)
	}
	conn, err := c.dialManagerWithRetry(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	request := &rpc.InjectRequest{Namespace: namespace, OwnerID: c.OwnerID, Image: image}
	for _, route := range routes {
		request.SyncRoutes = append(request.SyncRoutes, &rpc.SyncRoute{Workload: route.Workload, TargetWorkload: route.TargetWorkload})
	}
	stream, err := rpc.NewTunConfigServiceClient(conn).ProxyInject(ctx, request)
	if err != nil {
		return fmt.Errorf("server-side sync route inject: %w", err)
	}
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("server-side sync route inject: %w", recvErr)
		}
		if resp.GetMessage() != "" {
			plog.G(ctx).Info(resp.GetMessage())
		}
	}
	_ = c.getConfigMapStore().Refresh(ctx)
	return nil
}

// managerLeaveRetryBudget bounds admission of LeaveInject retries on transient errors,
// not the duration of an admitted dial or stream. Shorter than inject (15s vs 45s)
// since leave is a user-interactive operation.
const managerLeaveRetryBudget = 15 * time.Second

// leaveViaManager unpatches workloads server-side via the manager's LeaveInject RPC over
// the VPN. An empty workloads slice means "leave all workloads this OwnerID proxies in
// the namespace" (the manager derives them from ENVOY_CONFIG) — used by disconnect
// cleanup. Resolves the Service once, then retries transient dial and gRPC errors;
// LeaveInject is idempotent.
func (c *ConnectOptions) leaveViaManager(ctx context.Context, namespace string, workloads []string, ownerID string) error {
	deadline := time.Now().Add(managerLeaveRetryBudget)
	addr, err := c.resolveManagerAddr(ctx)
	if err != nil {
		return err
	}
	return c.leaveViaManagerAddr(ctx, addr, namespace, workloads, ownerID, deadline)
}

// leaveViaManagerAddr retries dial + LeaveInject without bounding the stream's context.
func (c *ConnectOptions) leaveViaManagerAddr(ctx context.Context, addr, namespace string, workloads []string, ownerID string, deadline time.Time) error {
	for attempt := 1; ; attempt++ {
		err := c.tryLeaveInject(ctx, addr, namespace, workloads, ownerID)
		if err == nil {
			return nil
		}
		if !isTransientGRPC(err) || ctx.Err() != nil || !time.Now().Add(managerDialRetryBackoff).Before(deadline) {
			return err
		}
		plog.G(ctx).Warnf("LeaveInject failed (attempt %d), retrying in %s: %v", attempt, managerDialRetryBackoff, err)
		select {
		case <-time.After(managerDialRetryBackoff):
		case <-ctx.Done():
			return err
		}
	}
}

// tryLeaveInject performs a single attempt of dial + LeaveInject stream.
func (c *ConnectOptions) tryLeaveInject(ctx context.Context, addr, namespace string, workloads []string, ownerID string) error {
	conn, err := c.dialManagerAddr(ctx, addr)
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

// isTransientGRPC reports whether err is a transient dial or gRPC error worth retrying.
func isTransientGRPC(err error) bool {
	code := status.Code(err)
	return errors.Is(err, context.DeadlineExceeded) || code == codes.Unavailable || code == codes.DeadlineExceeded
}
