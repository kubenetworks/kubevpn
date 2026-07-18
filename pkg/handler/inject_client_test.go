package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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
