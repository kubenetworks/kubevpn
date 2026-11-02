package syncworkload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type testLocalClient struct {
	stopping    chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	releaseOnce sync.Once
}

func (c *testLocalClient) Stop()                 { c.stopOnce.Do(func() { close(c.stopping) }); <-c.done }
func (c *testLocalClient) Done() <-chan struct{} { return c.done }
func (c *testLocalClient) Err() error            { return nil }
func (c *testLocalClient) release()              { c.releaseOnce.Do(func() { close(c.done) }) }

// Exercise real owner resolution, pod mutation, dynamic create/delete, ready-pod
// discovery, rollout watches, and runtime cancellation without starting the
// user's embedded Syncthing process or reading an ambient kubeconfig.
func TestRuntime_CloneReadyCancelCleanup(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("startFailure=%v", failStart), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client := fake.NewSimpleClientset()
			original := appsv1.Deployment{
				TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "test", Generation: 1, ResourceVersion: "1", UID: "original"},
				Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
					Template: v1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "app"}}, Spec: v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "original:v1"}}}}},
				Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
			}
			var mu sync.Mutex
			var clone *unstructured.Unstructured
			var events []string
			write := func(w http.ResponseWriter, obj any) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(obj)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Query().Get("watch") == "true" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-req.Context().Done()
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch req.URL.Path {
				case "/api":
					write(w, metav1.APIVersions{Versions: []string{"v1"}})
				case "/api/v1":
					write(w, metav1.APIResourceList{GroupVersion: "v1"})
				case "/apis":
					write(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"}}}})
				case "/apis/apps/v1":
					write(w, metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}})
				case "/apis/apps/v1/namespaces/test/deployments/app":
					events = append(events, "original-get")
					write(w, original)
				case "/apis/apps/v1/namespaces/test/deployments":
					if req.Method != http.MethodPost {
						events = append(events, "rollout")
						write(w, appsv1.DeploymentList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DeploymentList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.Deployment{original}})
						return
					}
					clone = &unstructured.Unstructured{}
					if err := json.NewDecoder(req.Body).Decode(clone); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					events = append(events, "create")
					_, err := client.CoreV1().Pods("test").Create(ctx, &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: "clone-pod", Namespace: "test", Labels: clone.GetLabels()},
						Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.2", Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}},
					}, metav1.CreateOptions{})
					if err != nil {
						t.Error(err)
					}
					write(w, clone)
				default:
					if clone != nil && strings.HasSuffix(req.URL.Path, "/"+clone.GetName()) {
						if req.Method == http.MethodDelete {
							events = append(events, "delete")
							write(w, metav1.Status{Status: metav1.StatusSuccess})
							return
						}
						write(w, clone)
						return
					}
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			factory := util.InitFactoryByBytes([]byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: test\ncurrent-context: test\n"), "test")
			cfg := Config{WorkloadNamespace: "test", ManagerNamespace: "manager", Workloads: []string{"deployments/app"}, Headers: map[string]string{"user": "alice"}, LocalDir: t.TempDir(), RemoteDir: "/app", TargetContainer: "app", TargetImage: "debug:v2"}
			r := New(ctx, cfg, client, factory, nil, nil)
			defer r.Stop()
			var workerCtx context.Context
			startErr := errors.New("syncthing startup failed")
			localHandle := &testLocalClient{stopping: make(chan struct{}), done: make(chan struct{})}
			defer localHandle.release()
			r.startClient = func(c context.Context, dir, local, remote string) (localClient, error) {
				workerCtx = c
				if dir != cfg.LocalDir || remote != "10.0.0.2:22000" || !strings.HasPrefix(local, "127.0.0.1:") {
					t.Errorf("unexpected client arguments: %s %s %s", dir, local, remote)
				}
				if failStart {
					return nil, startErr
				}
				return localHandle, nil
			}
			err := r.DoSync(ctx, []byte("test-kubeconfig"), "vpn:v1")
			if failStart && !errors.Is(err, startErr) || !failStart && err != nil {
				t.Fatalf("DoSync: %v", err)
			}
			mu.Lock()
			created := clone.DeepCopy()
			mu.Unlock()
			spec, _, err := clusterworkload.GetPodTemplateSpecPath(created)
			if err != nil {
				t.Fatal(err)
			}
			if created.GetUID() != "" || created.GetName() == original.Name || len(spec.Spec.Containers) != 3 {
				t.Fatalf("invalid clone: %v", created)
			}
			app := spec.Spec.Containers[0]
			if app.Image != cfg.TargetImage || app.Command[0] != "tail" || spec.Annotations[config.KUBECONFIG] != "test-kubeconfig" {
				t.Fatalf("clone configuration lost: %+v", spec)
			}
			vpn := spec.Spec.Containers[1]
			if value, ok := findFlagValue(vpn.Command, "--manager-namespace"); !ok || value != "manager" {
				t.Fatalf("manager namespace lost: %v", vpn.Command)
			}
			if value, ok := findFlagValue(vpn.Command, "--headers"); !ok || value != "user=alice" {
				t.Fatalf("headers lost: %v", vpn.Command)
			}
			targets := r.TargetWorkloadNames()
			if targets["deployments/app"] != "deployments/"+created.GetName() {
				t.Fatalf("missing clone status: %v", targets)
			}
			delete(targets, "deployments/app")
			if len(r.TargetWorkloadNames()) != 1 {
				t.Fatal("status snapshot aliases runtime state")
			}
			if !failStart && r.GetSyncthingGUIAddr() == "" {
				t.Fatal("GUI status missing")
			}
			r.cancel()
			if workerCtx == nil || workerCtx.Err() == nil {
				t.Fatal("local sync was not cancelled")
			}
			if ctx.Err() != nil {
				t.Fatal("runtime stop cancelled session")
			}
			cleanupDone := make(chan error, 1)
			go func() { cleanupDone <- r.Cleanup(ctx) }()
			if !failStart {
				select {
				case <-localHandle.stopping:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case err := <-cleanupDone:
					t.Fatalf("cleanup returned before local client exited: %v", err)
				default:
				}
				localHandle.release()
			}
			select {
			case err := <-cleanupDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			mu.Lock()
			defer mu.Unlock()
			deleteIndex, rolloutIndex := -1, -1
			for i, event := range events {
				if event == "delete" {
					deleteIndex = i
				}
				if event == "rollout" {
					rolloutIndex = i
				}
			}
			if deleteIndex < 0 || rolloutIndex <= deleteIndex {
				t.Fatalf("cleanup order: %v", events)
			}
		})
	}
}
