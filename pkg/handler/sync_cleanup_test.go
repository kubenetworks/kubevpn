//go:build !integration

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	logrus "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type fakeSyncRouteController struct {
	owner  string
	fail   bool
	record func(string)
}

func (f *fakeSyncRouteController) GetOwnerID() string { return f.owner }
func (f *fakeSyncRouteController) LeaveResource(_ context.Context, resources []Resources, _ string) error {
	for _, resource := range resources {
		f.record("leave:" + resource.Workload)
	}
	if f.fail {
		return errors.New("injected leave failure")
	}
	return nil
}

func TestSyncOptions_Cleanup_LeavesRoutesBeforeDeletingClones(t *testing.T) {
	for _, failLeave := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "leave failure preserves clone"}[failLeave], func(t *testing.T) {
			var mu sync.Mutex
			var events []string
			record := func(event string) {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
			}
			clone := &appsv1.Deployment{
				TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{Name: "authors-sync-a", Namespace: "test-ns"},
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
			})
			mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, metav1.APIResourceList{GroupVersion: "v1"}) })
			mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"}}}})
			})
			mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Namespaced: true, Kind: "Deployment"}}})
			})
			mux.HandleFunc("/apis/apps/v1/namespaces/test-ns/deployments/authors-sync-a", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					record("delete:deployments/authors-sync-a")
					writeJSON(w, metav1.Status{Status: metav1.StatusSuccess})
					return
				}
				writeJSON(w, clone)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			kubeconfigPath := t.TempDir() + "/kubeconfig"
			kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + srv.URL + `
  name: test
contexts:
- context:
    cluster: test
    namespace: test-ns
  name: test
current-context: test
`
			if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
				t.Fatal(err)
			}
			configFlags := genericclioptions.NewConfigFlags(true)
			configFlags.KubeConfig = &kubeconfigPath
			ns := "test-ns"
			configFlags.Namespace = &ns
			opts := &SyncOptions{
				WorkloadNamespace: ns, Workloads: []string{"deployments/authors"},
				TargetWorkloadNames: map[string]string{"deployments/authors": "deployments/authors-sync-a"},
			}
			if err := opts.InitClient(cmdutil.NewFactory(configFlags)); err != nil {
				t.Fatal(err)
			}
			controller := &fakeSyncRouteController{owner: "owner-a", fail: failLeave, record: record}
			opts.SetRouteController(controller)
			opts.AddRollbackFunc(func() error {
				record("rollback")
				return nil
			})
			err := opts.Cleanup(context.Background())
			if failLeave && err == nil {
				t.Fatal("expected route cleanup error")
			}
			if !failLeave && err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			leaveIndex, deleteIndex, rollbackIndex := -1, -1, -1
			for i, event := range events {
				if strings.HasPrefix(event, "leave:") && leaveIndex == -1 {
					leaveIndex = i
				}
				if strings.HasPrefix(event, "delete:") {
					deleteIndex = i
				}
				if event == "rollback" {
					rollbackIndex = i
				}
			}
			if leaveIndex == -1 || rollbackIndex == -1 {
				t.Fatalf("missing route cleanup or rollback: %v", events)
			}
			if failLeave {
				if deleteIndex != -1 {
					t.Fatalf("clone deleted after failed route cleanup: %v", events)
				}
				return
			}
			if deleteIndex == -1 || !(leaveIndex < deleteIndex && deleteIndex < rollbackIndex) {
				t.Fatalf("expected leave -> delete clone -> rollback, got %v", events)
			}
		})
	}
}

// TestSyncOptions_Cleanup_NilFactory_NoPanic pins the daemon-crash regression
// where Cleanup dereferenced a nil factory on the early-error path.
func TestSyncOptions_Cleanup_NilFactory_NoPanic(t *testing.T) {
	opts := &SyncOptions{
		WorkloadNamespace: "test-ns", Workloads: []string{"deployments/authors"},
		TargetWorkloadNames: map[string]string{"authors": "authors-clone"},
	}
	rolledBack := false
	opts.AddRollbackFunc(func() error { rolledBack = true; return nil })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Cleanup panicked with nil factory: %v", r)
		}
	}()
	if err := opts.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup with nil factory returned error: %v", err)
	}
	if !rolledBack {
		t.Fatal("rollback func was not executed")
	}
}

func TestSyncOptions_Cleanup_InitFailureStillRollsBack(t *testing.T) {
	opts := &SyncOptions{Workloads: []string{"deployments/app"}}
	rolledBack := false
	opts.AddRollbackFunc(func() error { rolledBack = true; return nil })
	factory := util.InitFactoryByPath(t.TempDir()+"/nonexistent-kubeconfig", "test")
	if err := opts.InitClient(factory); err == nil {
		t.Fatal("expected invalid kubeconfig initialization to fail")
	}
	if err := opts.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rolledBack || opts.runtime != nil {
		t.Fatal("failed initialization must unwind session without creating runtime")
	}
}

func TestSyncOptions_Cleanup_PreInitLogOrder(t *testing.T) {
	var file, stream bytes.Buffer
	logger := plog.GetLoggerForServer(int32(logrus.DebugLevel), &file)
	logger.AddHook(&plog.StreamHook{Writer: &stream, Level: logrus.InfoLevel})
	ctx := plog.WithLogger(context.Background(), logger)
	opts := &SyncOptions{}
	calls := 0
	opts.AddRollbackFunc(func() error {
		calls++
		plog.G(ctx).Info("rollback-session")
		return nil
	})
	if err := opts.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || opts.runtime != nil {
		t.Fatalf("rollback calls=%d, runtime=%v", calls, opts.runtime)
	}
	last := -1
	for _, message := range []string{"Stopping file sync", "Skipping workload cleanup: kubernetes client not initialized", "rollback-session", "Stopped file sync"} {
		index := strings.Index(file.String(), message)
		if index <= last {
			t.Fatalf("missing or out-of-order message %q: %s", message, file.String())
		}
		last = index
	}
	lines := strings.Split(strings.TrimSpace(stream.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected CLI stream: %q", stream.String())
	}
	if kind, text := plog.DecodeStep(lines[0]); kind != plog.StepBegin || text != "Stopping file sync" {
		t.Fatalf("missing CLI begin step: %q", lines[0])
	}
	if !strings.Contains(lines[1], "rollback-session") {
		t.Fatalf("rollback must precede CLI done: %q", stream.String())
	}
	if kind, text := plog.DecodeStep(lines[2]); kind != plog.StepEnd || text != "Stopped file sync" {
		t.Fatalf("missing CLI done step: %q", lines[2])
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
