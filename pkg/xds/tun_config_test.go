package xds

import (
	"context"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func newTestServer(t *testing.T) *TunConfigServer {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				config.KeyEnvoy:     "",
			},
		},
	)
	s, err := NewTunConfigServer(context.Background(), clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// 1. 基本分配：无 ExcludeIPs，首次调用分配新 IP
func TestNotifyIPChange_SyncsEnvoyRuleIP(t *testing.T) {
	ctx := context.Background()
	virtuals := []*Virtual{{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     "test-ns",
		UID:           "deployments.apps.reviews",
		Rules: []*Rule{{
			Headers:      map[string]string{},
			OwnerID:      "owner-1",
			LocalTunIPv4: "198.18.0.5",
			LocalTunIPv6: "2001:2::5",
		}},
	}}
	envoyYAML, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatalf("marshal envoy: %v", err)
	}
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: string(envoyYAML)},
		},
	)
	s, err := NewTunConfigServer(ctx, clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}

	newV4 := &net.IPNet{IP: net.ParseIP("198.18.0.9"), Mask: net.CIDRMask(16, 32)}
	newV6 := &net.IPNet{IP: net.ParseIP("2001:2::9"), Mask: net.CIDRMask(64, 128)}
	s.NotifyIPChange("owner-1", newV4, newV6)

	// syncEnvoyRuleIP runs asynchronously; poll for the update.
	deadline := time.Now().Add(2 * time.Second)
	for {
		cm, _ := clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		vs, perr := parseYaml(cm.Data[config.KeyEnvoy])
		got := ""
		if perr == nil && len(vs) == 1 && len(vs[0].Rules) == 1 {
			got = vs[0].Rules[0].LocalTunIPv4
		}
		if got == "198.18.0.9" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("envoy rule IP not synced after NotifyIPChange, got %q want 198.18.0.9", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGetTunIP_HostnamePersistedToConfigMap exercises the full hostname lifecycle:
// a client passes its machine name to GetTunIP over real gRPC, the server records it
// in the TUN_ALLOCS ConfigMap, and a restarted server restores it via loadAllocs.
// It also asserts that a client which sends no hostname leaves a clean entry (omitempty).
