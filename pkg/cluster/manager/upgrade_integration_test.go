package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// upgradeFactory uses real kubectl deployment/rollout and typed HTTP requests.
// The local server stores TLS and Service updates in a fake clientset; no ambient
// kubeconfig is consulted.
type upgradeFactory struct {
	cmdutil.Factory
	clientset *kubernetes.Clientset
}

func (f upgradeFactory) KubernetesClientSet() (*kubernetes.Clientset, error) {
	return f.clientset, nil
}

func TestManagerUpgrade_TLSDeploymentServiceLifecycle(t *testing.T) {
	for _, failRollout := range []bool{false, true} {
		name := "upgrade and rotate TLS"
		if failRollout {
			name = "failed rollout attempts undo and leaves service unchanged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			const ns = "team"
			dep := genDeploySpec(ns, "old-image", "registry-credentials", config.AttachModeSidecar)
			dep.APIVersion, dep.Kind = "apps/v1", "Deployment"
			dep.ResourceVersion, dep.Generation = "1", 1
			dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
			secret := genSecret(ns, []byte("legacy-cert"), []byte("legacy-key"), nil)
			secret.ResourceVersion = "7"
			svc := genService(ns)
			svc.Spec.Ports = svc.Spec.Ports[:1]
			svc.ResourceVersion = "9"
			cs := fake.NewSimpleClientset(secret, svc)
			var mu sync.Mutex
			var patched, undo bool
			writeJSON := func(w http.ResponseWriter, value any) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/namespaces/team/secrets/" + config.ConfigMapPodTrafficManager:
					if r.Method == http.MethodPut {
						var value v1.Secret
						if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if _, err := cs.CoreV1().Secrets(ns).Update(ctx, &value, metav1.UpdateOptions{}); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
					}
					value, err := cs.CoreV1().Secrets(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					value.APIVersion, value.Kind = "v1", "Secret"
					writeJSON(w, value)
				case "/api/v1/namespaces/team/services/" + config.ConfigMapPodTrafficManager:
					if r.Method == http.MethodPut {
						var value v1.Service
						if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if _, err := cs.CoreV1().Services(ns).Update(ctx, &value, metav1.UpdateOptions{}); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
					}
					value, err := cs.CoreV1().Services(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					value.APIVersion, value.Kind = "v1", "Service"
					writeJSON(w, value)
				case "/api":
					writeJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
				case "/api/v1":
					writeJSON(w, metav1.APIResourceList{GroupVersion: "v1"})
				case "/apis":
					writeJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"}}}})
				case "/apis/apps/v1":
					writeJSON(w, metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}, {Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"}}})
				default:
					if strings.Contains(r.URL.Path, "/daemonsets") {
						ds := genNodeAgentDaemonSet(ns, "new-image")
						ds.APIVersion, ds.Kind = "apps/v1", "DaemonSet"
						writeJSON(w, ds)
						return
					}
					if strings.Contains(r.URL.Path, "/replicasets") {
						undo = true
						writeJSON(w, appsv1.ReplicaSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}})
						return
					}
					if !strings.Contains(r.URL.Path, "/deployments") {
						http.NotFound(w, r)
						return
					}
					if r.URL.Query().Get("watch") != "" {
						return
					}
					if r.Method == http.MethodPatch {
						patch, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						original, err := json.Marshal(dep)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						updated, err := strategicpatch.StrategicMergePatch(original, patch, appsv1.Deployment{})
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if err := json.Unmarshal(updated, dep); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						patched = true
						if failRollout {
							dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: v1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
						}
					}
					if strings.HasSuffix(r.URL.Path, "/deployments") {
						writeJSON(w, appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.Deployment{*dep}})
						return
					}
					writeJSON(w, dep)
				}
			}))
			defer srv.Close()
			flags := genericclioptions.NewConfigFlags(true)
			dummy := filepath.Join(t.TempDir(), "kubeconfig")
			if err := os.WriteFile(dummy, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0600); err != nil {
				t.Fatal(err)
			}
			flags.KubeConfig = &dummy
			flags.APIServer = &srv.URL
			flags.Namespace = new(string)
			*flags.Namespace = ns
			httpClient, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
			if err != nil {
				t.Fatal(err)
			}
			factory := upgradeFactory{Factory: cmdutil.NewFactory(flags), clientset: httpClient}
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			err = Upgrade(bounded, factory, ns, "new-image")
			if (err != nil) != failRollout {
				t.Fatalf("Upgrade error = %v, failRollout = %v", err, failRollout)
			}
			mu.Lock()
			gotPatch, gotUndo, result := patched, undo, dep.DeepCopy()
			mu.Unlock()
			if !gotPatch || gotUndo != failRollout {
				t.Fatalf("patch=%v undo=%v", gotPatch, gotUndo)
			}
			for _, container := range result.Spec.Template.Spec.Containers {
				if container.Image != "new-image" {
					t.Fatalf("container %s image=%s", container.Name, container.Image)
				}
			}
			if result.Spec.Template.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
				t.Fatal("lost image pull secret")
			}
			updated, err := cs.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if updated.ResourceVersion != "7" || len(updated.Data[config.TLSServerName]) == 0 {
				t.Fatal("TLS upgrade lost resource version or server name")
			}
			currentSvc, err := cs.CoreV1().Services(ns).Get(ctx, svc.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantPorts := 4
			if failRollout {
				wantPorts = 1
			}
			if len(currentSvc.Spec.Ports) != wantPorts || currentSvc.ResourceVersion != "9" {
				t.Fatalf("unexpected service: %+v", currentSvc)
			}
			// A complete secret is preserved on repeat upgrades; explicit rotation
			// replaces it using the same resource-version-aware implementation.
			if err := upgradeSecretSpec(ctx, factory, ns); err != nil {
				t.Fatal(err)
			}
			preserved, err := cs.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(updated.Data[config.TLSCertKey], preserved.Data[config.TLSCertKey]) {
				t.Fatal("complete TLS certificate unexpectedly changed")
			}
			if err := RotateTLS(ctx, cs, ns); err != nil {
				t.Fatal(err)
			}
			rotated, err := cs.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(updated.Data[config.TLSCertKey], rotated.Data[config.TLSCertKey]) {
				t.Fatal("explicit TLS rotation did not replace certificate")
			}
		})
	}
}

func TestManagerImages_RollbackAndPodSelection(t *testing.T) {
	ctx := context.Background()
	const ns = "team"
	dep := genDeploySpec(ns, "desired-image", "", config.AttachModeSidecar)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: ns, Labels: map[string]string{"app": config.ConfigMapPodTrafficManager}}, Spec: dep.Spec.Template.Spec, Status: v1.PodStatus{Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}, ContainerStatuses: []v1.ContainerStatus{{Ready: true}}}}
	pod.Spec.Containers[0].Image = "rollback-image"
	// The pod and deployment must not share the container backing array.
	dep = genDeploySpec(ns, "desired-image", "", config.AttachModeSidecar)
	cs := fake.NewSimpleClientset(dep, pod)
	desired, running, err := Images(ctx, cs, ns)
	if err != nil || desired != "desired-image" || running != "rollback-image" {
		t.Fatalf("Images=%q,%q,%v", desired, running, err)
	}
	pod.Status.Conditions[0].Status = v1.ConditionFalse
	if _, err := cs.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Images(ctx, cs, ns); err == nil {
		t.Fatal("unready manager must not be selected")
	}
}

func TestManagerUpgrade_PreservesAttachMode(t *testing.T) {
	ctx := context.Background()
	const ns = "team"

	tests := []struct {
		name         string
		existingEnv  string
		expectedEnv  string
	}{
		{
			name:        "preserve existing node-agent mode",
			existingEnv: config.AttachModeNodeAgent,
			expectedEnv: config.AttachModeNodeAgent,
		},
		{
			name:        "preserve existing sidecar mode",
			existingEnv: config.AttachModeSidecar,
			expectedEnv: config.AttachModeSidecar,
		},
		{
			name:        "default to node-agent when no env exists",
			existingEnv: "",
			expectedEnv: config.AttachModeNodeAgent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create deployment with initial state
			dep := genDeploySpec(ns, "old-image", "", config.AttachModeSidecar)
			dep.APIVersion, dep.Kind = "apps/v1", "Deployment"
			dep.ResourceVersion, dep.Generation = "1", 1
			dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}

			// Set or clear the attach mode env
			if tt.existingEnv != "" {
				for i := range dep.Spec.Template.Spec.Containers {
					if dep.Spec.Template.Spec.Containers[i].Name == config.ContainerSidecarVPN {
						for j := range dep.Spec.Template.Spec.Containers[i].Env {
							if dep.Spec.Template.Spec.Containers[i].Env[j].Name == "KUBEVPN_PROXY_ATTACH_MODE" {
								dep.Spec.Template.Spec.Containers[i].Env[j].Value = tt.existingEnv
							}
						}
					}
				}
			} else {
				// Remove the env entirely
				for i := range dep.Spec.Template.Spec.Containers {
					if dep.Spec.Template.Spec.Containers[i].Name == config.ContainerSidecarVPN {
						var newEnv []v1.EnvVar
						for _, e := range dep.Spec.Template.Spec.Containers[i].Env {
							if e.Name != "KUBEVPN_PROXY_ATTACH_MODE" {
								newEnv = append(newEnv, e)
							}
						}
						dep.Spec.Template.Spec.Containers[i].Env = newEnv
					}
				}
			}

			secret := genSecret(ns, []byte("cert"), []byte("key"), []byte("host"))
			secret.ResourceVersion = "7"
			svc := genService(ns)
			svc.ResourceVersion = "9"
			cs := fake.NewSimpleClientset(secret, svc)
			var mu sync.Mutex
			writeJSON := func(w http.ResponseWriter, value any) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/namespaces/team/secrets/" + config.ConfigMapPodTrafficManager:
					value, err := cs.CoreV1().Secrets(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					value.APIVersion, value.Kind = "v1", "Secret"
					writeJSON(w, value)
				case "/api/v1/namespaces/team/services/" + config.ConfigMapPodTrafficManager:
					value, err := cs.CoreV1().Services(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					value.APIVersion, value.Kind = "v1", "Service"
					writeJSON(w, value)
				case "/api":
					writeJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
				case "/api/v1":
					writeJSON(w, metav1.APIResourceList{GroupVersion: "v1"})
				case "/apis":
					writeJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"}}}})
				case "/apis/apps/v1":
					writeJSON(w, metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}, {Name: "replicasets", Namespaced: true, Kind: "ReplicaSet"}}})
				default:
					if strings.Contains(r.URL.Path, "/daemonsets") {
						ds := genNodeAgentDaemonSet(ns, "new-image")
						ds.APIVersion, ds.Kind = "apps/v1", "DaemonSet"
						writeJSON(w, ds)
						return
					}
					if strings.Contains(r.URL.Path, "/replicasets") {
						writeJSON(w, appsv1.ReplicaSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}})
						return
					}
					if !strings.Contains(r.URL.Path, "/deployments") {
						http.NotFound(w, r)
						return
					}
					if r.URL.Query().Get("watch") != "" {
						return
					}
					if r.Method == http.MethodPatch {
						patch, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						original, err := json.Marshal(dep)
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						updated, err := strategicpatch.StrategicMergePatch(original, patch, appsv1.Deployment{})
						if err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						if err := json.Unmarshal(updated, dep); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
					}
					if strings.HasSuffix(r.URL.Path, "/deployments") {
						writeJSON(w, appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.Deployment{*dep}})
						return
					}
					writeJSON(w, dep)
				}
			}))
			defer srv.Close()
			flags := genericclioptions.NewConfigFlags(true)
			dummy := filepath.Join(t.TempDir(), "kubeconfig")
			if err := os.WriteFile(dummy, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0600); err != nil {
				t.Fatal(err)
			}
			flags.KubeConfig = &dummy
			flags.APIServer = &srv.URL
			flags.Namespace = new(string)
			*flags.Namespace = ns
			httpClient, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
			if err != nil {
				t.Fatal(err)
			}
			factory := upgradeFactory{Factory: cmdutil.NewFactory(flags), clientset: httpClient}
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			err = Upgrade(bounded, factory, ns, "new-image")
			if err != nil {
				t.Fatalf("Upgrade error = %v", err)
			}
			mu.Lock()
			result := dep.DeepCopy()
			mu.Unlock()

			// Verify the attach mode was preserved
			found := false
			for _, container := range result.Spec.Template.Spec.Containers {
				if container.Name == config.ContainerSidecarVPN {
					for _, env := range container.Env {
						if env.Name == "KUBEVPN_PROXY_ATTACH_MODE" {
							if env.Value != tt.expectedEnv {
								t.Fatalf("expected KUBEVPN_PROXY_ATTACH_MODE=%q, got %q", tt.expectedEnv, env.Value)
							}
							found = true
						}
					}
				}
			}
			if !found && tt.expectedEnv != "" {
				t.Fatalf("expected KUBEVPN_PROXY_ATTACH_MODE=%q, but env not found", tt.expectedEnv)
			}
		})
	}
}
