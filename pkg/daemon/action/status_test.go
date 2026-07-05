package action

import (
	"os"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func TestExtractLocalPorts_ColonSeparated(t *testing.T) {
	m := map[int32]string{
		8080: "29450:19080",
	}
	result := extractLocalPorts(m)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[8080] != 19080 {
		t.Errorf("expected 8080 → 19080, got %d", result[8080])
	}
}

func TestExtractLocalPorts_PlainNumber(t *testing.T) {
	m := map[int32]string{
		8080: "9080",
	}
	result := extractLocalPorts(m)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[8080] != 9080 {
		t.Errorf("expected 8080 → 9080, got %d", result[8080])
	}
}

func TestExtractLocalPorts_Empty(t *testing.T) {
	result := extractLocalPorts(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty map for nil input, got %d entries", len(result))
	}

	result = extractLocalPorts(map[int32]string{})
	if len(result) != 0 {
		t.Fatalf("expected empty map for empty input, got %d entries", len(result))
	}
}

func TestExtractLocalPorts_InvalidPort(t *testing.T) {
	m := map[int32]string{
		8080: "invalid",
	}
	result := extractLocalPorts(m)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[8080] != 0 {
		t.Errorf("expected 8080 → 0 for invalid port, got %d", result[8080])
	}
}

func TestExtractLocalPorts_MultipleEntries(t *testing.T) {
	m := map[int32]string{
		80:   "30000:8080",
		443:  "30001:8443",
		3000: "5000",
	}
	result := extractLocalPorts(m)
	if len(result) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(result))
	}
	expected := map[int32]int32{
		80:   8080,
		443:  8443,
		3000: 5000,
	}
	for k, want := range expected {
		if got := result[k]; got != want {
			t.Errorf("key %d: expected %d, got %d", k, want, got)
		}
	}
}

func TestExtractLocalPorts_ColonWithInvalidSecond(t *testing.T) {
	m := map[int32]string{
		8080: "29450:notanumber",
	}
	result := extractLocalPorts(m)
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

	status := buildConnectionStatus(conn)
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

	status := buildConnectionStatus(conn)
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
