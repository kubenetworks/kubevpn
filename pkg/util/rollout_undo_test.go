//go:build !integration

package util

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/utils/ptr"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// rollout_undo_test.go pins the leave/unpatch regression: RolloutStatus's failure
// path runs `kubectl rollout undo`, which reverts a workload to its PREVIOUS
// revision. On the inject path that is a deliberate safety net, but on the
// unpatch/leave/reset path the previous revision still carries the sidecar, so the
// undo re-applies exactly what we just removed and leaves the workload dirty
// (observed in CI as authors keeping its envoy+vpn sidecars after leave, then a
// later test mis-routing). The undoOnFailure flag gates that behavior.
//
// The test wires a real kubectl Factory to an httptest apiserver. The target
// deployment never converges (observedGeneration < generation) and the watch
// hangs, so RolloutStatus fails with DeadlineExceeded under a shortened timeout.
// `rollout undo` for a Deployment lists ReplicaSets to find the target revision;
// RolloutStatus itself never touches ReplicaSets. So a ReplicaSet request is a
// clean, reliable signal that undo ran.
func TestRolloutStatus_UndoOnFailure(t *testing.T) {
	tests := []struct {
		name          string
		undoOnFailure bool
		wantUndo      bool
	}{
		{"unpatch/leave path does not undo", false, false},
		{"inject path undoes to restore working revision", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			replicaSetRequested := false

			ns := "test-ns"
			dep := &appsv1.Deployment{
				TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{Name: "authors", Namespace: ns, Generation: 2, ResourceVersion: "1"},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To[int32](1),
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "authors"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "authors"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "authors", Image: "authors:latest"}}},
					},
				},
				// observedGeneration (1) < generation (2) => rollout never "done".
				Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 0},
			}

			mux := http.NewServeMux()
			mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
				writeRolloutJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
			})
			mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
				writeRolloutJSON(w, metav1.APIResourceList{GroupVersion: "v1"})
			})
			mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
				writeRolloutJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{
					Name:             "apps",
					Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}},
					PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
				}}})
			})
			mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, r *http.Request) {
				writeRolloutJSON(w, metav1.APIResourceList{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{Name: "deployments", Namespaced: true, Kind: "Deployment"},
						{Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"},
					},
				})
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				// The undo signal: `rollout undo` lists ReplicaSets; RolloutStatus never does.
				if strings.Contains(path, "/replicasets") {
					mu.Lock()
					replicaSetRequested = true
					mu.Unlock()
					writeRolloutJSON(w, appsv1.ReplicaSetList{
						TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"},
						ListMeta: metav1.ListMeta{ResourceVersion: "1"},
					})
					return
				}
				if strings.Contains(path, "/deployments") {
					// Watch: return no events. RolloutStatus's WatchFunc is bound to the
					// outer (never-cancelled) context, so block for a bounded time rather
					// than on r.Context(); the reflector exits once UntilWithSync's own
					// timeout fires and closes the informer stop channel.
					if r.URL.Query().Get("watch") != "" {
						select {
						case <-r.Context().Done():
						case <-time.After(time.Second):
						}
						return
					}
					// Collection LIST (informer sync).
					if strings.HasSuffix(path, "/deployments") {
						writeRolloutJSON(w, appsv1.DeploymentList{
							TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"},
							ListMeta: metav1.ListMeta{ResourceVersion: "1"},
							Items:    []appsv1.Deployment{*dep},
						})
						return
					}
					// Single GET (builder .Latest() and undo's Complete()).
					writeRolloutJSON(w, dep)
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

			// Fail fast instead of waiting the production 2 minutes.
			orig := config.RolloutStatusTimeout
			config.RolloutStatusTimeout = 300 * time.Millisecond
			t.Cleanup(func() { config.RolloutStatusTimeout = orig })

			err := RolloutStatus(context.Background(), factory, ns, "deployments/authors", tt.undoOnFailure)
			if err == nil {
				t.Fatal("expected RolloutStatus to fail (rollout never converges), got nil")
			}

			mu.Lock()
			gotUndo := replicaSetRequested
			mu.Unlock()
			if gotUndo != tt.wantUndo {
				t.Fatalf("undo happened=%v, want %v (undoOnFailure=%v)", gotUndo, tt.wantUndo, tt.undoOnFailure)
			}
		})
	}
}

func writeRolloutJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
