package handler

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// leaveTestService exercises the real streaming RPC, including request ownership,
// progress delivery, terminal status and EOF.
type leaveTestService struct {
	rpc.UnimplementedTunConfigServiceServer
	t       *testing.T
	calls   atomic.Int32
	respond func(int32, rpc.TunConfigService_LeaveInjectServer) error
}

func (s *leaveTestService) LeaveInject(req *rpc.InjectRequest, stream rpc.TunConfigService_LeaveInjectServer) error {
	attempt := s.calls.Add(1)
	if req.Namespace != "workloads" || req.OwnerID != "owner-a" || !slices.Equal(req.Workloads, []string{"deployments.apps/foo"}) {
		s.t.Errorf("unexpected leave request: %v", req)
		return status.Error(codes.InvalidArgument, "unexpected request")
	}
	if err := stream.Send(&rpc.InjectResponse{Message: "removing owner rules"}); err != nil {
		return err
	}
	if s.respond != nil {
		return s.respond(attempt, stream)
	}
	return nil
}

type leaveTestListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *leaveTestListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

type leaveTestEnv struct {
	client  *ConnectOptions
	addr    string
	service *leaveTestService
	listen  *leaveTestListener
	started chan struct{}
	closed  chan error
}

// newLeaveTestEnv resolves a fake Service's host but uses the real listener's
// ephemeral port. With stallFirst, the first transport consumes the HTTP/2 client
// preface without replying, then starts gRPC only after that client closes it.
func newLeaveTestEnv(t *testing.T, stallFirst bool, respond func(int32, rpc.TunConfigService_LeaveInjectServer) error) *leaveTestEnv {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env := &leaveTestEnv{
		listen:  &leaveTestListener{Listener: listener},
		service: &leaveTestService{t: t, respond: respond},
		started: make(chan struct{}),
		closed:  make(chan error, 1),
	}
	server := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(server, env.service)
	var mu sync.Mutex
	var stalled net.Conn
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		if stalled != nil {
			_ = stalled.Close()
		}
		mu.Unlock()
		server.Stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("leave test server did not stop")
		}
	})
	go func() {
		defer close(done)
		if stallFirst {
			// Holding the lock through Accept ensures cleanup cannot miss a
			// connection accepted just as it closes the listener.
			mu.Lock()
			conn, acceptErr := env.listen.Accept()
			stalled = conn
			mu.Unlock()
			if acceptErr != nil {
				return
			}
			defer conn.Close()
			preface := make([]byte, 24)
			if _, err := io.ReadFull(conn, preface); err != nil {
				env.closed <- err
				return
			}
			close(env.started)
			_, err := io.Copy(io.Discard, conn) // nil means the client sent EOF.
			_ = conn.Close()
			env.closed <- err
			if err != nil {
				return
			}
		}
		_ = server.Serve(env.listen)
	}()
	cs := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Spec:       v1.ServiceSpec{ClusterIP: "127.0.0.1"},
	})
	env.client = &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
		ManagerNamespace: "kubevpn",
	}
	addr, err := env.client.resolveManagerAddr(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	env.addr = net.JoinHostPort(host, port)
	if len(cs.Actions()) != 1 || !cs.Actions()[0].Matches("get", "services") {
		t.Fatalf("expected one Service GET, got %v", cs.Actions())
	}
	return env
}

func (e *leaveTestEnv) leave(ctx context.Context, deadline time.Time) error {
	return e.client.leaveViaManagerAddr(ctx, e.addr, "workloads", []string{"deployments.apps/foo"}, "owner-a", deadline)
}

func TestLeaveViaManager_DialDeadlineThenRecovery(t *testing.T) {
	env := newLeaveTestEnv(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := env.leave(ctx, start.Add(managerLeaveRetryBudget))
	if err != nil {
		t.Fatalf("leave must recover after the initial dial deadline: %v (gRPC code %s, context deadline %t)", err, status.Code(err), errors.Is(err, context.DeadlineExceeded))
	}
	select {
	case err := <-env.closed:
		if err != nil {
			t.Fatalf("stalled connection did not close with EOF: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stalled connection was not closed")
	}
	if env.listen.accepted.Load() != 2 || env.service.calls.Load() != 1 {
		t.Fatalf("want stalled dial then one successful RPC; connections=%d RPCs=%d", env.listen.accepted.Load(), env.service.calls.Load())
	}
	if elapsed := time.Since(start); elapsed < managerDialTimeout+managerDialRetryBackoff {
		t.Fatalf("dial timeout and retry backoff were shortened: %s", elapsed)
	}
}

func TestLeaveViaManager_ServiceLookupFailsFast(t *testing.T) {
	for _, name := range []string{"missing", "headless", "empty", "deadline"} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if name == "headless" || name == "empty" {
				ip := ""
				if name == "headless" {
					ip = v1.ClusterIPNone
				}
				cs = fake.NewSimpleClientset(&v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
					Spec:       v1.ServiceSpec{ClusterIP: ip},
				})
			}
			if name == "deadline" {
				cs.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, context.DeadlineExceeded
				})
			}
			c := &ConnectOptions{
				SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
				ManagerNamespace: "kubevpn",
			}
			start := time.Now()
			err := c.leaveViaManager(context.Background(), "workloads", nil, "owner-a")
			if err == nil {
				t.Fatal("expected Service lookup failure")
			}
			if name == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Service GET deadline was not preserved: %v", err)
			}
			if elapsed := time.Since(start); elapsed >= managerDialRetryBackoff {
				t.Errorf("Service lookup error did not fail immediately: %s", elapsed)
			}
			if actions := cs.Actions(); len(actions) != 1 || !actions[0].Matches("get", "services") {
				t.Fatalf("want exactly one Service GET, got %v", actions)
			}
		})
	}
}

func TestLeaveViaManager_CancelDuringDial(t *testing.T) {
	env := newLeaveTestEnv(t, true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- env.leave(ctx, time.Now().Add(managerLeaveRetryBudget))
	}()
	// Also join the client on an early test failure; server cleanup runs afterwards.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("leave client did not stop")
		}
	})
	select {
	case <-env.started:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not send HTTP/2 preface")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want wrapped caller cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop the stalled dial promptly")
	}
	select {
	case err := <-env.closed:
		if err != nil {
			t.Fatalf("cancelled dial did not close with EOF: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled dial connection remained open")
	}
	if env.listen.accepted.Load() != 1 || env.service.calls.Load() != 0 {
		t.Fatalf("cancelled dial retried: connections=%d RPCs=%d", env.listen.accepted.Load(), env.service.calls.Load())
	}
}

func TestLeaveViaManager_RPCStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      codes.Code
		exhausted bool
		wantCalls int32
	}{
		{name: "permission denied", code: codes.PermissionDenied, wantCalls: 1},
		{name: "unavailable", code: codes.Unavailable, wantCalls: 2},
		{name: "deadline exceeded", code: codes.DeadlineExceeded, wantCalls: 2},
		{name: "retry budget exhausted", code: codes.Unavailable, exhausted: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLeaveTestEnv(t, false, func(attempt int32, _ rpc.TunConfigService_LeaveInjectServer) error {
				if attempt == 1 {
					return status.Error(tc.code, "first attempt failed")
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			deadline := start.Add(managerLeaveRetryBudget)
			if tc.exhausted {
				deadline = start
			}
			err := env.leave(ctx, deadline)
			if tc.wantCalls == 2 && err != nil {
				t.Fatalf("transient RPC did not recover: %v", err)
			}
			if tc.wantCalls == 1 && status.Code(err) != tc.code {
				t.Fatalf("want original %s error, got %v", tc.code, err)
			}
			if env.service.calls.Load() != tc.wantCalls {
				t.Fatalf("want %d RPCs, got %d", tc.wantCalls, env.service.calls.Load())
			}
			elapsed := time.Since(start)
			if tc.wantCalls == 1 && elapsed >= managerDialRetryBackoff {
				t.Errorf("non-retried error did not fail fast: %s", elapsed)
			}
			if tc.wantCalls == 2 && elapsed < managerDialRetryBackoff {
				t.Errorf("retry skipped backoff: %s", elapsed)
			}
		})
	}
}

func TestLeaveViaManager_StreamKeepsCallerContext(t *testing.T) {
	env := newLeaveTestEnv(t, false, func(_ int32, stream rpc.TunConfigService_LeaveInjectServer) error {
		// Outlive the dial timeout and the already-expired retry admission budget.
		timer := time.NewTimer(managerDialTimeout + time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return stream.Send(&rpc.InjectResponse{Message: "leave complete"})
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := env.leave(ctx, time.Now()); err != nil {
		t.Fatalf("stream must retain caller context beyond dial timeout and retry admission budget: %v", err)
	}
	if env.service.calls.Load() != 1 {
		t.Fatalf("successful stream retried: %d RPCs", env.service.calls.Load())
	}
}

// TestManagerDial_FailsWithoutClusterIP verifies that, with injection now server-side
// only (no local fallback), a missing / headless / empty-ClusterIP traffic manager
// Service makes the inject and leave paths return an error rather than silently
// succeeding. No cluster required.
func TestManagerDial_FailsWithoutClusterIP(t *testing.T) {
	cases := []struct {
		name string
		svc  *v1.Service
	}{
		{name: "no manager service"},
		{name: "headless manager service", svc: &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
			Spec:       v1.ServiceSpec{ClusterIP: v1.ClusterIPNone},
		}},
		{name: "empty clusterIP", svc: &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
			Spec:       v1.ServiceSpec{ClusterIP: ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cs *fake.Clientset
			if tc.svc != nil {
				cs = fake.NewSimpleClientset(tc.svc)
			} else {
				cs = fake.NewSimpleClientset()
			}
			c := &ConnectOptions{
				SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
				ManagerNamespace: "kubevpn",
			}
			if err := c.createRemoteInboundViaManager(context.Background(), "kubevpn", nil, nil, "img", "198.18.0.5", "", []string{"deployments.apps/foo"}); err == nil {
				t.Fatal("inject: expected error when manager is unreachable, got nil")
			}
			if err := c.leaveViaManager(context.Background(), "kubevpn", []string{"deployments.apps/foo"}, "owner-a"); err == nil {
				t.Fatal("leave: expected error when manager is unreachable, got nil")
			}
		})
	}
}

// TestManagerDial_PermanentErrorFailsFast guards the split between resolveManagerAddr
// (permanent config errors) and the retried dial: a missing / no-ClusterIP manager must
// fail immediately, NOT retry for managerDialRetryBudget. Without the split the inject
// retry loop would spin ~45s on an error that can never succeed.
func TestManagerDial_PermanentErrorFailsFast(t *testing.T) {
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: fake.NewSimpleClientset()}},
		ManagerNamespace: "kubevpn",
	}
	start := time.Now()
	err := c.createRemoteInboundViaManager(context.Background(), "kubevpn", nil, nil, "img", "198.18.0.5", "", []string{"deployments.apps/foo"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error for missing manager service, got nil")
	}
	if elapsed >= managerDialRetryBackoff {
		t.Fatalf("permanent error must fail fast, but took %s (>= one retry backoff %s) — it was retried", elapsed, managerDialRetryBackoff)
	}
}

// TestManagerDial_RetryHonorsContext verifies that with a valid-but-unroutable ClusterIP
// the dial (not the address resolution) is what fails, that the error is wrapped with the
// dial address, and that a cancelled context stops the retry loop promptly rather than
// blocking for the full managerDialRetryBudget.
func TestManagerDial_RetryHonorsContext(t *testing.T) {
	cs := fake.NewSimpleClientset(&v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		// TEST-NET-3 (RFC 5737) address — routable-looking but guaranteed unreachable.
		Spec: v1.ServiceSpec{ClusterIP: "203.0.113.1"},
	})
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
		ManagerNamespace: "kubevpn",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.dialManagerWithRetry(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected dial error for unreachable ClusterIP, got nil")
	}
	if !strings.Contains(err.Error(), "dial traffic manager 203.0.113.1:") {
		t.Fatalf("error must be the wrapped dial failure, got: %v", err)
	}
	// The 300ms ctx must cut the retry loop well short of the 45s budget.
	if elapsed >= managerDialRetryBudget {
		t.Fatalf("cancelled context must stop retries early, but took %s (>= budget %s)", elapsed, managerDialRetryBudget)
	}
}

// TestResolveManagerAddr_UnreachableClusterFailsFast guards the teardown-speed fix: the
// manager Service GET must honour config.ManagerServiceGetTimeout instead of inheriting
// client-go's ~30s default dial timeout, so a disconnect/quit against an unreachable
// cluster (e.g. deleted) fails fast rather than stalling. Uses an unroutable apiserver
// (TEST-NET-3, RFC 5737) — no real cluster or kubeconfig involved.
func TestResolveManagerAddr_UnreachableClusterFailsFast(t *testing.T) {
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: "https://203.0.113.1:6443"})
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
		ManagerNamespace: "kubevpn",
	}
	start := time.Now()
	_, err = c.resolveManagerAddr(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error resolving manager addr against unreachable cluster, got nil")
	}
	// Must be governed by ManagerServiceGetTimeout, not the ~30s client-go dial default.
	if elapsed >= 2*config.ManagerServiceGetTimeout {
		t.Fatalf("GET must fail fast under ManagerServiceGetTimeout (%s), but took %s", config.ManagerServiceGetTimeout, elapsed)
	}
}
