package xds

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// writeOwnerRule seeds ENVOY_CONFIG with a single rule owned by owner.
func writeOwnerRule(t *testing.T, env *testEnv, owner string) {
	t.Helper()
	ctx := context.Background()
	cm, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyEnvoy] = fmt.Sprintf(`- schemaVersion: 2
  Uid: deployments.apps.web
  namespace: test-ns
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "1.2.3.4"
    ownerID: "%s"
    portmap:
      8080: "9080"
`, owner)
	if _, err = env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func ownerRuleExists(t *testing.T, env *testEnv, owner string) bool {
	t.Helper()
	cm, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
	if err != nil {
		return false
	}
	for _, v := range virtuals {
		for _, rule := range v.Rules {
			if rule.OwnerID == owner {
				return true
			}
		}
	}
	return false
}

// TestIntegration_AbandonmentTTL_RemovesRule verifies that a rule is kept across a normal
// lease reap (sleep) but removed once the owner has been gone longer than abandonmentTTL.
func TestIntegration_AbandonmentTTL_RemovesRule(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	owner := "abandon-user"

	if _, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns"}); err != nil {
		t.Fatal(err)
	}
	writeOwnerRule(t, env, owner)

	// Expire the lease and reap. The rule must SURVIVE (this is the sleep window).
	env.server.mu.Lock()
	env.server.allocs[owner].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	if !ownerRuleExists(t, env, owner) {
		t.Fatal("rule removed on plain lease reap — sleep/wake would break")
	}
	env.server.mu.Lock()
	_, marked := env.server.reapedAt[owner]
	env.server.mu.Unlock()
	if !marked {
		t.Fatal("reapedAt not recorded after reap")
	}

	// Backdate the reap time beyond the TTL, then reap again: now abandoned → rule removed.
	env.server.mu.Lock()
	env.server.reapedAt[owner] = time.Now().Add(-abandonmentTTL - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	if ownerRuleExists(t, env, owner) {
		t.Fatal("rule not removed after abandonmentTTL elapsed")
	}
}

// TestIntegration_AbandonmentTTL_WakePreservesRule verifies sleep/wake fidelity: if the
// owner re-acquires (GetTunIP) before the abandonment pass, its rule is preserved even
// though the lease had been reaped and the reap time is old.
func TestIntegration_AbandonmentTTL_WakePreservesRule(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	owner := "sleepy-user"

	if _, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns"}); err != nil {
		t.Fatal(err)
	}
	writeOwnerRule(t, env, owner)

	// Reap, then backdate the reap time beyond the TTL...
	env.server.mu.Lock()
	env.server.allocs[owner].LastRenew = time.Now().Add(-LeaseDuration - time.Minute)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)
	env.server.mu.Lock()
	env.server.reapedAt[owner] = time.Now().Add(-abandonmentTTL - time.Minute)
	env.server.mu.Unlock()

	// ...but the owner WAKES and re-acquires before the next reap pass.
	if _, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns"}); err != nil {
		t.Fatal(err)
	}
	env.server.reapExpiredLeases(ctx)

	if !ownerRuleExists(t, env, owner) {
		t.Fatal("rule removed despite owner re-acquiring (sleep/wake) — GetTunIP must clear reapedAt")
	}
}
