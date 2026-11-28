package action

import (
	"context"
	"os"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func TestPortMapToLocalPorts_ColonSeparated(t *testing.T) {
	rule := &controlplane.Rule{
		PortMap: map[int32]string{
			8080: "29450:19080",
		},
	}
	result := portMapToLocalPorts(rule)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[8080] != 19080 {
		t.Errorf("expected 8080 → 19080, got %d", result[8080])
	}
}

func TestPortMapToLocalPorts_PlainNumber(t *testing.T) {
	rule := &controlplane.Rule{
		PortMap: map[int32]string{
			8080: "9080",
		},
	}
	result := portMapToLocalPorts(rule)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	// Node-agent plain values are the local listening port.
	if result[8080] != 9080 {
		t.Errorf("expected 8080 → 9080, got %d", result[8080])
	}
}

func TestPortMapToLocalPorts_Empty(t *testing.T) {
	result := portMapToLocalPorts(&controlplane.Rule{PortMap: nil})
	if len(result) != 0 {
		t.Fatalf("expected empty map for nil input, got %d entries", len(result))
	}

	result = portMapToLocalPorts(&controlplane.Rule{PortMap: map[int32]string{}})
	if len(result) != 0 {
		t.Fatalf("expected empty map for empty input, got %d entries", len(result))
	}
}

func TestPortMapToLocalPorts_InvalidPort(t *testing.T) {
	rule := &controlplane.Rule{
		PortMap: map[int32]string{
			8080: "invalid",
		},
	}
	result := portMapToLocalPorts(rule)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	// Invalid node-agent values have no local listening port.
	if result[8080] != 0 {
		t.Errorf("expected 8080 → 0 for invalid port, got %d", result[8080])
	}
}

func TestPortMapToLocalPorts_MultipleEntries(t *testing.T) {
	rule := &controlplane.Rule{
		PortMap: map[int32]string{
			80:   "30000:8080",
			443:  "30001:8443",
			3000: "5000",
		},
	}
	result := portMapToLocalPorts(rule)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	expected := map[int32]int32{
		80:   8080,
		443:  8443,
		3000: 5000, // node-agent plain number → LocalPort = EnvoyPort
	}
	for k, want := range expected {
		if got := result[k]; got != want {
			t.Errorf("key %d: expected %d, got %d", k, want, got)
		}
	}
}

func TestPortMapToLocalPorts_ColonWithInvalidSecond(t *testing.T) {
	rule := &controlplane.Rule{
		PortMap: map[int32]string{
			8080: "29450:notanumber",
		},
	}
	result := portMapToLocalPorts(rule)
	if result[8080] != 0 {
		t.Errorf("expected 8080 → 0 for non-numeric after colon, got %d", result[8080])
	}
}

func TestBuildConnectionStatus_NilFactory(t *testing.T) {
	// buildConnectionStatus with nil factory should not panic; GetKubeconfigCluster
	// will be called with nil which would panic. Use a real factory.
	tmpKubeconfig := t.TempDir() + "/kubeconfig"
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: default
  name: test-context
current-context: test-context
`
	if err := os.WriteFile(tmpKubeconfig, []byte(kubeconfigContent), 0644); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &tmpKubeconfig
	ns := "default"
	configFlags.Namespace = &ns
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	conn := handler.NewConnectOptionsForTest(factory, tmpKubeconfig, "default")

	status := buildConnectionStatus(conn, nil)
	if status == nil {
		t.Fatal("buildConnectionStatus returned nil")
	}
	// No TUN device, no DHCP → status should be "disconnected"
	if status.Status != StatusFailed {
		t.Errorf("expected status %q, got %q", StatusFailed, status.Status)
	}
	if status.Kubeconfig != tmpKubeconfig {
		t.Errorf("expected kubeconfig %q, got %q", tmpKubeconfig, status.Kubeconfig)
	}
	if status.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %q", status.Namespace)
	}
	if status.Cluster != "test-cluster" {
		t.Errorf("expected cluster 'test-cluster', got %q", status.Cluster)
	}
	// ConnectionID is empty because dhcp is nil
	if status.ConnectionID != "" {
		t.Errorf("expected empty ConnectionID, got %q", status.ConnectionID)
	}
	// Netif is empty because no TUN device exists
	if status.Netif != "" {
		t.Errorf("expected empty Netif, got %q", status.Netif)
	}
}

func TestBuildConnectionStatus_FieldMapping(t *testing.T) {
	tmpKubeconfig := t.TempDir() + "/kubeconfig"
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://10.0.0.1:6443
  name: prod-cluster
contexts:
- context:
    cluster: prod-cluster
    namespace: kube-system
  name: prod-context
current-context: prod-context
`
	if err := os.WriteFile(tmpKubeconfig, []byte(kubeconfigContent), 0644); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &tmpKubeconfig
	ns := "kube-system"
	configFlags.Namespace = &ns
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	conn := handler.NewConnectOptionsForTest(factory, tmpKubeconfig, "kube-system")

	status := buildConnectionStatus(conn, nil)
	if status.Cluster != "prod-cluster" {
		t.Errorf("expected cluster 'prod-cluster', got %q", status.Cluster)
	}
	if status.Namespace != "kube-system" {
		t.Errorf("expected namespace 'kube-system', got %q", status.Namespace)
	}
	if status.Kubeconfig != tmpKubeconfig {
		t.Errorf("expected kubeconfig path %q, got %q", tmpKubeconfig, status.Kubeconfig)
	}
}

type statusSnapshotConnection struct {
	*handler.ConnectOptions
	envoy string
}

func (c statusSnapshotConnection) GetClusterName() string { return "cluster" }
func (c statusSnapshotConnection) GetOriginKubeconfigPath() string {
	return ""
}

func (c statusSnapshotConnection) StatusSnapshot(context.Context) (handler.SessionStatus, error) {
	return handler.SessionStatus{Envoy: c.envoy}, nil
}

func TestBuildProxyAndSyncStatus_UsesDesiredVirtuals(t *testing.T) {
	conn := statusSnapshotConnection{
		ConnectOptions: &handler.ConnectOptions{OwnerID: "owner"},
		envoy: `- Namespace: default
  Uid: deployments.apps.reviews
  Rules:
  - ownerID: owner
    PortMap:
      "9080": "8080"
`,
	}
	proxies, _, err := buildProxyAndSyncStatus(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || proxies[0].Workload != "deployments.apps/reviews" {
		t.Fatalf("unexpected desired workload status: %+v", proxies)
	}
	if len(proxies[0].RuleList) != 1 || !proxies[0].RuleList[0].CurrentDevice || proxies[0].RuleList[0].PortMap[9080] != 8080 {
		t.Fatalf("desired rule was not rendered correctly: %+v", proxies[0].RuleList)
	}
}

func TestBuildProxyAndSyncStatus_SyncRouteIsNotCurrentDevice(t *testing.T) {
	conn := statusSnapshotConnection{
		ConnectOptions: &handler.ConnectOptions{OwnerID: "owner"},
		envoy: `- Namespace: default
  Uid: deployments.apps.reviews
  Rules:
  - ownerID: owner
    syncTarget:
      uid: deployments.apps.reviews-sync-a
      selector: owner-ref=reviews-sync-a
`,
	}
	proxies, _, err := buildProxyAndSyncStatus(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 1 || len(proxies[0].RuleList) != 1 || proxies[0].RuleList[0].CurrentDevice {
		t.Fatalf("sync clone route must not be reported as the current device: %+v", proxies)
	}
}
