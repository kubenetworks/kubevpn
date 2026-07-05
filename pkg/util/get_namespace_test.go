package util

import (
	"os"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"
)

// newFactoryFromKubeconfig writes kubeconfig content to a temp file and returns
// a factory pointing at that file. The ambient default kubeconfig is never used.
func newFactoryFromKubeconfig(t *testing.T, content string) cmdutil.Factory {
	t.Helper()
	tmp, err := os.CreateTemp("", "kubeconfig-*.yaml")
	if err != nil {
		t.Fatalf("create temp kubeconfig: %v", err)
	}
	tmp.Close()
	t.Cleanup(func() { _ = os.Remove(tmp.Name()) })
	if err = os.WriteFile(tmp.Name(), []byte(content), 0600); err != nil {
		t.Fatalf("write temp kubeconfig: %v", err)
	}
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = ptr.To(tmp.Name())
	return cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
}

// kubeconfig with an explicit namespace in the active context.
const kubeconfigWithNamespace = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://localhost:8001
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: my-namespace
  name: test-context
current-context: test-context
users: []
`

// kubeconfig whose context has no namespace field — should fall back to "default".
const kubeconfigWithoutNamespace = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: http://localhost:8001
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
  name: test-context
current-context: test-context
users: []
`

// TestGetNamespace_ExplicitNamespace verifies that GetNamespace returns the
// namespace explicitly set in the kubeconfig context.
func TestGetNamespace_ExplicitNamespace(t *testing.T) {
	factory := newFactoryFromKubeconfig(t, kubeconfigWithNamespace)
	got, err := GetNamespace(factory)
	if err != nil {
		t.Fatalf("GetNamespace returned unexpected error: %v", err)
	}
	if got != "my-namespace" {
		t.Errorf("expected namespace %q, got %q", "my-namespace", got)
	}
}

// TestGetNamespace_DefaultNamespace verifies that GetNamespace returns "default"
// when the kubeconfig context does not specify a namespace.
func TestGetNamespace_DefaultNamespace(t *testing.T) {
	factory := newFactoryFromKubeconfig(t, kubeconfigWithoutNamespace)
	got, err := GetNamespace(factory)
	if err != nil {
		t.Fatalf("GetNamespace returned unexpected error: %v", err)
	}
	if got != "default" {
		t.Errorf("expected namespace %q, got %q", "default", got)
	}
}

// TestGetNamespace_NonexistentKubeconfig verifies that GetNamespace returns an
// error when the kubeconfig file path does not exist.
func TestGetNamespace_NonexistentKubeconfig(t *testing.T) {
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = ptr.To("/nonexistent/path/to/kubeconfig.yaml")
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	_, err := GetNamespace(factory)
	if err == nil {
		t.Fatal("expected an error for a nonexistent kubeconfig, got nil")
	}
}
