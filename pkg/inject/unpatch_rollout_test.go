//go:build !integration

package inject

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/scheme"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// unpatch_rollout_test.go covers the leave/unpatch path end-to-end through the
// inject package: UnpatchContainer -> patchWorkload -> RolloutStatus. patchWorkload
// must pass undoOnFailure=false so a timed-out rollback does NOT run `rollout undo`
// and re-apply the sidecar we just removed. This is the exact CI regression where
// authors kept its envoy+vpn sidecars after leave and a later run-mesh test
// mis-routed.
//
// The test wires a real kubectl Factory to an httptest apiserver, resolves the
// deployment into a resource.Info, and calls patchWorkload directly. The rollout
// never converges (observedGeneration < generation) so RolloutStatus fails under a
// shortened timeout. We assert the unpatch PATCH was applied and that no ReplicaSet
// request (the undo signal) was made.
func TestPatchWorkload_UnpatchDoesNotUndoOnRolloutTimeout(t *testing.T) {
	var mu sync.Mutex
	patched := false
	replicaSetRequested := false

	ns := "test-ns"
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "authors", Namespace: ns, Generation: 2, ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "authors"}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "authors"}},
				Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "authors", Image: "authors:latest"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 0},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		writeUnpatchJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		writeUnpatchJSON(w, metav1.APIResourceList{GroupVersion: "v1"})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		writeUnpatchJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{
			Name:             "apps",
			Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
		}}})
	})
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, r *http.Request) {
		writeUnpatchJSON(w, metav1.APIResourceList{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
			},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/replicasets") {
			mu.Lock()
			replicaSetRequested = true
			mu.Unlock()
			writeUnpatchJSON(w, appsv1.ReplicaSetList{
				TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			})
			return
		}
		if strings.Contains(path, "/deployments") {
			if r.Method == http.MethodPatch {
				mu.Lock()
				patched = true
				mu.Unlock()
				writeUnpatchJSON(w, dep)
				return
			}
			if r.URL.Query().Get("watch") != "" {
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
				return
			}
			if strings.HasSuffix(path, "/deployments") {
				writeUnpatchJSON(w, appsv1.DeploymentList{
					TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"},
					ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					Items:    []appsv1.Deployment{*dep},
				})
				return
			}
			writeUnpatchJSON(w, dep)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	kubeconfigPath := t.TempDir() + "/kubeconfig"
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + srv.URL + `
  name: c
contexts:
- context:
    cluster: c
    namespace: ` + ns + `
  name: ctx
current-context: ctx
`
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &kubeconfigPath
	configFlags.Namespace = &ns
	factory := cmdutil.NewFactory(configFlags)

	// Resolve the deployment into a resource.Info (gives info.Client + info.Mapping).
	r := factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
		ResourceTypeOrNameArgs(true, "deployments/authors").
		SingleResourceType().Latest().Do()
	infos, err := r.Infos()
	if err != nil {
		t.Fatalf("resolve deployment: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 info, got %d", len(infos))
	}

	orig := config.RolloutStatusTimeout
	config.RolloutStatusTimeout = 300 * time.Millisecond
	t.Cleanup(func() { config.RolloutStatusTimeout = orig })

	// The "unpatched" template (sidecar already removed).
	templateSpec := &v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "authors"}},
		Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "authors", Image: "authors:latest"}}},
	}

	// undoOnFailure=false: this is what mesh.go UnpatchContainer passes.
	err = patchWorkload(context.Background(), factory, infos[0], templateSpec, []string{"spec", "template"}, false)
	if err == nil {
		t.Fatal("expected patchWorkload to surface the rollout timeout, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if !patched {
		t.Fatal("unpatch PATCH was never applied to the deployment")
	}
	if replicaSetRequested {
		t.Fatal("rollout undo ran on the unpatch path (undoOnFailure=false must not undo)")
	}
}

func writeUnpatchJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
