package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// newFakeK8sFactory starts an httptest server that serves the minimum
// Kubernetes API discovery endpoints so that cmdutil.Factory methods
// (ToRESTConfig, RESTClient, KubernetesClientSet) succeed without a
// real cluster. It returns the factory and a cleanup function.
func newFakeK8sFactory(t *testing.T) (cmdutil.Factory, func()) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metav1.APIVersions{
			Versions: []string{"v1"},
		})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metav1.APIResourceList{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
				{Name: "pods", Namespaced: true, Kind: "Pod"},
			},
		})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metav1.APIGroupList{})
	})
	// Catch-all for version negotiation (/version, /openapi/v2, etc.)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})

	srv := httptest.NewServer(mux)

	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + srv.URL + `
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: test-ns
  name: test-context
current-context: test-context
`
	tmpFile := t.TempDir() + "/kubeconfig"
	if err := os.WriteFile(tmpFile, []byte(kubeconfigContent), 0644); err != nil {
		srv.Close()
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &tmpFile
	ns := "test-ns"
	configFlags.Namespace = &ns
	factory := cmdutil.NewFactory(configFlags)

	return factory, srv.Close
}

func TestK8sClient_InitClient(t *testing.T) {
	factory, cleanup := newFakeK8sFactory(t)
	defer cleanup()

	var k K8sClient
	ns, err := k.InitClient(factory)
	if err != nil {
		t.Fatalf("InitClient returned error: %v", err)
	}

	if ns != "test-ns" {
		t.Errorf("expected namespace %q, got %q", "test-ns", ns)
	}
	if k.factory == nil {
		t.Error("expected factory to be set, got nil")
	}
	if k.config == nil {
		t.Error("expected config to be set, got nil")
	}
	if k.restclient == nil {
		t.Error("expected restclient to be set, got nil")
	}
	if k.clientset == nil {
		t.Error("expected clientset to be set, got nil")
	}
}

func TestK8sClient_GetFactory(t *testing.T) {
	factory, cleanup := newFakeK8sFactory(t)
	defer cleanup()

	var k K8sClient
	if _, err := k.InitClient(factory); err != nil {
		t.Fatalf("InitClient returned error: %v", err)
	}

	got := k.GetFactory()
	if got == nil {
		t.Fatal("GetFactory returned nil")
	}
	if got != factory {
		t.Error("GetFactory returned a different factory than the one passed to InitClient")
	}
}

func TestK8sClient_GetClientset(t *testing.T) {
	factory, cleanup := newFakeK8sFactory(t)
	defer cleanup()

	var k K8sClient
	if _, err := k.InitClient(factory); err != nil {
		t.Fatalf("InitClient returned error: %v", err)
	}

	cs := k.GetClientset()
	if cs == nil {
		t.Fatal("GetClientset returned nil")
	}
	// Verify the clientset is functional by checking it is the same
	// interface value stored internally.
	if cs != k.clientset {
		t.Error("GetClientset returned a different clientset than the one stored internally")
	}
}
