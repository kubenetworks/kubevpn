package handler

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Watcher logs are written by background goroutines while the test observes them.
type watcherLog struct {
	mu sync.Mutex
	strings.Builder
}

func (b *watcherLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Builder.Write(p)
}

func (b *watcherLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Builder.String()
}

func watcherContext(t *testing.T, level logrus.Level) (context.Context, context.CancelFunc, *watcherLog) {
	t.Helper()
	buf := &watcherLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForServer(int32(level), buf)))
	t.Cleanup(cancel)
	return ctx, cancel, buf
}

func awaitWatcher(t *testing.T, timeout time.Duration, reason string, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for !ready() {
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", reason)
		}
	}
}

type watcherRun struct {
	done    chan struct{}
	err     error
	version int64
}

func runWatcher(t *testing.T, cancel context.CancelFunc, run func(*int64) error) *watcherRun {
	t.Helper()
	r := &watcherRun{done: make(chan struct{})}
	go func() {
		r.err = run(&r.version)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("watcher did not stop after cancellation")
		}
	})
	return r
}

func (r *watcherRun) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(timeout):
		t.Fatal("watcher did not return within its time budget")
	}
}

type watcherCase struct {
	name string
	once func(*NetworkManager, context.Context, *int64) error
	loop func(*NetworkManager, context.Context)
}

var watcherCases = []watcherCase{
	{"RouteWatcher", (*NetworkManager).doWatchNamespaceRoutes, (*NetworkManager).watchNamespaceRoutes},
	{"IPWatcher", (*NetworkManager).doWatchTunIP, (*NetworkManager).watchTunIPFromXDS},
}

// Seed real DHCP state so IP pushes don't need a privileged OS address change.
func prepareWatcher(t *testing.T, env *networkTestEnv) (*NetworkManager, <-chan string) {
	t.Helper()
	nm := newTestNetworkManager(t, env, "watcher-owner")
	nm.cfg.WorkloadNamespace = "test-ns"
	resp, err := env.server.GetTunIP(context.Background(), &rpc.TunIPRequest{OwnerID: nm.cfg.OwnerID, Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	nm.localTunIPv4, nm.localTunIPv6 = parseTunIPResponse(resp)
	nm.tunName = "test-watcher-tun"
	routes := make(chan string, 32)
	nm.newRouteResolver = func() routeResolver { return nil }
	nm.addTunRoutes = func(_ string, added ...types.Route) error {
		for _, route := range added {
			routes <- route.Dst.String()
		}
		return nil
	}
	createWatcherService(t, env, "web", "10.96.0.10")
	return nm, routes
}

func createWatcherService(t *testing.T, env *networkTestEnv, name, ip string) {
	t.Helper()
	_, err := env.clientset.CoreV1().Services("test-ns").Create(context.Background(), &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{ClusterIP: ip, ClusterIPs: []string{ip}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func waitWatcherReady(t *testing.T, tc watcherCase, env *networkTestEnv, routes <-chan string) {
	t.Helper()
	if tc.name == "IPWatcher" {
		awaitWatcher(t, 5*time.Second, "IP subscription", func() bool { return env.server.WatcherCount("watcher-owner") == 1 })
		return
	}
	select {
	case route := <-routes:
		if route != "10.96.0.10/32" {
			t.Fatalf("snapshot route = %s", route)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route snapshot was not applied")
	}
}

// Removing the dial's own deadline must fail even though the test has a cleanup
// watchdog: the caller is a live session with no deadline, just as in the daemon.
func TestIntegration_WatcherDialTimeoutAndRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range watcherCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newPendingNetworkTestEnv(t)
			nm, routes := prepareWatcher(t, env)
			ctx, cancel, _ := watcherContext(t, logrus.InfoLevel)
			target := env.listener.Addr().String()
			started := time.Now()
			r := runWatcher(t, cancel, func(version *int64) error { return tc.once(nm, ctx, version) })
			r.wait(t, 40*time.Second)
			if !errors.Is(r.err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("dial error = %v, parent error = %v", r.err, ctx.Err())
			}
			if elapsed := time.Since(started); elapsed < 29*time.Second || elapsed > 40*time.Second {
				t.Fatalf("dial returned after %s, expected its own 30s budget", elapsed)
			}
			if !strings.Contains(r.err.Error(), target) {
				t.Fatalf("dial error lacks target: %v", r.err)
			}
			go env.grpcServer.Serve(env.listener)
			recovered := runWatcher(t, cancel, func(_ *int64) error { tc.loop(nm, ctx); return nil })
			waitWatcherReady(t, tc, env, routes)
			cancel()
			recovered.wait(t, 5*time.Second)
		})
	}
}

// This drives the actual outer loop, rather than letting gRPC's internal retry
// make an unbounded dial look like a successful application-level recovery.
func TestIntegration_WatcherRetryRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range watcherCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newPendingNetworkTestEnv(t)
			nm, routes := prepareWatcher(t, env)
			ctx, cancel, logs := watcherContext(t, logrus.InfoLevel)
			r := runWatcher(t, cancel, func(_ *int64) error { tc.loop(nm, ctx); return nil })
			awaitWatcher(t, 40*time.Second, "deadline warning", func() bool {
				return strings.Contains(logs.String(), "context deadline exceeded")
			})
			out := logs.String()
			for _, want := range []string{"warning:", tc.name, env.listener.Addr().String(), "test-ns", "retrying in 10s"} {
				if !strings.Contains(out, want) {
					t.Errorf("normal-level log lacks %q: %s", want, out)
				}
			}
			retryStart := time.Now()
			go env.grpcServer.Serve(env.listener)
			if tc.name == "IPWatcher" {
				awaitWatcher(t, 15*time.Second, "recovered IP subscription", func() bool { return env.server.WatcherCount(nm.cfg.OwnerID) == 1 })
			} else {
				select {
				case route := <-routes:
					if route != "10.96.0.10/32" {
						t.Errorf("recovered route = %s", route)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("route watcher did not recover")
				}
			}
			if time.Since(retryStart) < 9*time.Second {
				t.Fatal("outer retry interval was bypassed")
			}
			cancel()
			r.wait(t, 5*time.Second)
		})
	}
}

func TestIntegration_WatcherStreamOutlivesDialTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range watcherCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			streamExited := make(chan struct{}, 1)
			env := newPendingNetworkTestEnv(t, grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
				defer func() { streamExited <- struct{}{} }()
				return handler(srv, stream)
			}))
			go env.grpcServer.Serve(env.listener)
			nm, routes := prepareWatcher(t, env)
			processed := make(chan struct{}, 16)
			nm.cfg.ReservedTunIPs = func() []net.IP { processed <- struct{}{}; return nil }
			ctx, cancel, logs := watcherContext(t, logrus.DebugLevel)
			r := runWatcher(t, cancel, func(version *int64) error { return tc.once(nm, ctx, version) })
			waitWatcherReady(t, tc, env, routes)
			select {
			case <-r.done:
				t.Fatalf("healthy watch exited: %v", r.err)
			case <-time.After(31 * time.Second):
			}
			if tc.name == "RouteWatcher" {
				createWatcherService(t, env, "later", "10.96.0.20")
				select {
				case route := <-routes:
					if route != "10.96.0.20/32" {
						t.Errorf("delta route = %s", route)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("delta not applied after dial deadline")
				}
			} else {
				for len(processed) > 0 {
					<-processed
				}
				env.server.NotifyIPChange(nm.cfg.OwnerID, nm.localTunIPv4, nm.localTunIPv6)
				select {
				case <-processed:
				case <-time.After(5 * time.Second):
					t.Fatal("IP push not processed after dial deadline")
				}
			}
			cancel()
			r.wait(t, 5*time.Second)
			if r.version == 0 {
				t.Fatal("no version received")
			}
			for _, want := range []string{"dial", "connected", env.listener.Addr().String()} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("debug log lacks %q: %s", want, logs.String())
				}
			}
			select {
			case <-streamExited:
			case <-time.After(5 * time.Second):
				t.Fatal("server watch handler did not exit after cancellation")
			}
		})
	}
}

func TestIntegration_WatcherCancelAndLogs(t *testing.T) {
	t.Parallel()
	for _, tc := range watcherCases {
		for _, phase := range []string{"dial", "retry", "stream"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				t.Parallel()
				entered := make(chan struct{}, 1)
				env := newPendingNetworkTestEnv(t, grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
					if phase == "retry" {
						entered <- struct{}{}
						return status.Error(codes.Unavailable, "injected disconnect")
					}
					return handler(srv, stream)
				}))
				nm, routes := prepareWatcher(t, env)
				ctx, cancel, logs := watcherContext(t, logrus.DebugLevel)
				if phase != "dial" {
					go env.grpcServer.Serve(env.listener)
				}
				r := runWatcher(t, cancel, func(_ *int64) error { tc.loop(nm, ctx); return nil })
				switch phase {
				case "dial":
					awaitWatcher(t, 5*time.Second, "dial attempt", func() bool { return strings.Contains(logs.String(), "dial") })
				case "retry":
					awaitWatcher(t, 5*time.Second, "retry warning", func() bool { return strings.Contains(logs.String(), "retrying") })
				case "stream":
					waitWatcherReady(t, tc, env, routes)
				}
				warnings := strings.Count(logs.String(), "warning:")
				cancel()
				r.wait(t, time.Second)
				if got := strings.Count(logs.String(), "warning:"); got != warnings {
					t.Errorf("shutdown added warnings: %s", logs.String())
				}
			})
		}
	}
	t.Run("waiting-for-generation", func(t *testing.T) {
		ctx, cancel, logs := watcherContext(t, logrus.DebugLevel)
		nm := newNetworkManager(NetworkConfig{OwnerID: "owner"})
		t.Cleanup(nm.Stop)
		nm.StartRouteWatcher(ctx)
		nm.StartIPWatcher(ctx)
		awaitWatcher(t, time.Second, "watcher startup diagnostics", func() bool {
			return strings.Contains(logs.String(), "[RouteWatcher]") && strings.Contains(logs.String(), "[IPWatcher]")
		})
		cancel()
		nm.Stop()
		if strings.Contains(logs.String(), "warning:") {
			t.Errorf("waiting for a generation or cancellation added warnings: %s", logs.String())
		}
	})
	t.Run("missing-owner", func(t *testing.T) {
		ctx, _, logs := watcherContext(t, logrus.DebugLevel)
		nm := newNetworkManager(NetworkConfig{})
		nm.StartIPWatcher(ctx)
		if !strings.Contains(logs.String(), "OwnerID") {
			t.Errorf("missing skip reason: %s", logs.String())
		}
	})
}

func TestIntegration_RouteWatcherDiscoveryRecovery(t *testing.T) {
	t.Parallel()
	for _, denied := range []bool{true, false} {
		name := "unavailable"
		if denied {
			name = "forbidden"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newNetworkTestEnv(t)
			var failing atomic.Bool
			failing.Store(true)
			env.clientset.PrependReactor("list", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
				if !failing.Load() {
					return false, nil, nil
				}
				if denied {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", nil)
				}
				return true, nil, apierrors.NewServiceUnavailable("injected API failure")
			})
			// Install reactors before DHCP starts its asynchronous ConfigMap work.
			nm, routes := prepareWatcher(t, env)
			ctx, cancel, logs := watcherContext(t, logrus.InfoLevel)
			r := runWatcher(t, cancel, func(_ *int64) error { nm.watchNamespaceRoutes(ctx); return nil })
			awaitWatcher(t, 5*time.Second, "discovery retry warning", func() bool { return strings.Contains(logs.String(), "retrying in 10s") })
			select {
			case route := <-routes:
				t.Fatalf("failed discovery applied route %s", route)
			default:
			}
			failing.Store(false)
			select {
			case route := <-routes:
				if route != "10.96.0.10/32" {
					t.Fatalf("recovered route = %s", route)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("discovery did not recover in the existing session")
			}
			awaitWatcher(t, time.Second, "snapshot summary", func() bool {
				return strings.Contains(logs.String(), "snapshot") && strings.Contains(logs.String(), "1 services")
			})
			cancel()
			r.wait(t, time.Second)
		})
	}
}

func TestIntegration_IPWatcherSelfHealTimeout(t *testing.T) {
	t.Parallel()
	callDone := make(chan time.Time, 1)
	env := newPendingNetworkTestEnv(t, grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasSuffix(info.FullMethod, "/GetTunIP") {
			deadline, _ := ctx.Deadline()
			<-ctx.Done()
			// The client's deadline may send RST_STREAM before the server's timer
			// fires, so server ctx.Err() can legitimately be context.Canceled.
			callDone <- deadline
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return handler(ctx, req)
	}))
	nm, _ := prepareWatcher(t, env)
	go env.grpcServer.Serve(env.listener)
	ctx, cancel, logs := watcherContext(t, logrus.InfoLevel)
	started := time.Now()
	r := runWatcher(t, cancel, func(version *int64) error { return nm.doWatchTunIP(ctx, version) })
	select {
	case deadline := <-callDone:
		if budget := deadline.Sub(started); budget < 9*time.Second || budget > 12*time.Second {
			t.Fatalf("self-heal RPC deadline budget = %s, want 10s", budget)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("self-heal RPC has no independent timeout")
	}
	if elapsed := time.Since(started); elapsed < 9*time.Second {
		t.Fatalf("self-heal returned too early: %s", elapsed)
	}
	awaitWatcher(t, 5*time.Second, "fallback IP subscription", func() bool { return env.server.WatcherCount(nm.cfg.OwnerID) == 1 })
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if !strings.Contains(logs.String(), "GetTunIP") || !strings.Contains(logs.String(), "warning:") {
		t.Errorf("missing self-heal warning: %s", logs.String())
	}
	cancel()
	r.wait(t, 5*time.Second)
}
