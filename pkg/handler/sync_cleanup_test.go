//go:build !integration

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	logrus "github.com/sirupsen/logrus"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// TestSyncOptions_Cleanup_RollbackRunsAfterRolloutWait is an integration-style
// test that pins the ordering bug where the SSH tunnel was torn down before the
// rollout-status wait that still needs it.
//
// In production the only rollback func registered (daemon/action/sync.go) calls
// session.RunCleanups() -> session.Cancel(), which cancels the context that the
// SSH port-forward (pkg/ssh/ssh.go PortMapUntil) is bound to, closing the local
// tunnel endpoint that d.factory's kubeconfig points at. If Cleanup runs that
// rollback func BEFORE waiting for the original workload's rollout, the rollout
// request hits a closed tunnel and fails with "connection refused".
//
// This test simulates the tunnel with a real httptest apiserver and a rollback
// func that closes it. It asserts the rollout request reaches the server (tunnel
// still open) and that the rollback (tunnel close) happens afterwards.
func TestSyncOptions_Cleanup_RollbackRunsAfterRolloutWait(t *testing.T) {
	var logOutput bytes.Buffer
	logger := plog.GetLoggerForServer(int32(logrus.DebugLevel), &logOutput)
	logCtx := plog.WithLogger(context.Background(), logger)
	var mu sync.Mutex
	var events []string
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	mux := http.NewServeMux()
	// Minimal discovery so the resource builder can resolve apps/v1 deployments.
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, metav1.APIVersions{Versions: []string{"v1"}})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, metav1.APIResourceList{GroupVersion: "v1"})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, metav1.APIGroupList{Groups: []metav1.APIGroup{{
			Name:             "apps",
			Versions:         []metav1.GroupVersionForDiscovery{{GroupVersion: "apps/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "apps/v1", Version: "v1"},
		}}})
	})
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, metav1.APIResourceList{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
			},
		})
	})
	// Catch-all: record any request that targets the rollout workload, then return
	// 404 so RolloutStatus fails fast (before its 2-minute watch) without hanging.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "deployments/rollout-target") {
			record("rollout-request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(&metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Reason:   metav1.StatusReasonNotFound,
			Code:     http.StatusNotFound,
		})
	})

	srv := httptest.NewServer(mux)
	closeOnce := sync.Once{}
	closeSrv := func() { closeOnce.Do(srv.Close) }
	defer closeSrv()

	kubeconfig := `apiVersion: v1
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
	kubeconfigPath := t.TempDir() + "/kubeconfig"
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &kubeconfigPath
	ns := "test-ns"
	configFlags.Namespace = &ns
	factory := cmdutil.NewFactory(configFlags)

	opts := &SyncOptions{
		WorkloadNamespace: ns,
		Workloads:         []string{"deployments/rollout-target"},
	}
	if err := opts.InitClient(factory); err != nil {
		t.Fatalf("InitClient: %v", err)
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	opts.SetContext(workerCtx)
	// Build the runtime with a live session context; Cleanup must stop its child
	// context without cancelling the session before the rollout requests finish.
	r := opts.syncRuntime(workerCtx)

	// Simulate the production rollback func: tearing down the SSH tunnel by
	// closing the apiserver endpoint. This MUST run after the rollout wait.
	opts.AddRollbackFunc(func() error {
		if workerCtx.Err() != nil {
			t.Error("runtime cleanup cancelled the session context")
		}
		if err := r.DoSync(workerCtx, nil, ""); !errors.Is(err, context.Canceled) {
			t.Errorf("runtime must be stopped before session teardown: %v", err)
		}
		plog.G(logCtx).Info("rollback-close-tunnel")
		record("rollback-close-tunnel")
		closeSrv()
		return nil
	})

	// No delete targets — the test is purely about the rollout-vs-rollback order.
	_ = opts.Cleanup(logCtx)
	lastLogIndex := -1
	for _, message := range []string{"Stopping file sync", "rollback-close-tunnel", "Stopped file sync for 0 workloads"} {
		index := strings.Index(logOutput.String(), message)
		if index <= lastLogIndex {
			t.Fatalf("missing or out-of-order cleanup log %q: %s", message, logOutput.String())
		}
		lastLogIndex = index
	}

	mu.Lock()
	defer mu.Unlock()
	rolloutIdx, rollbackIdx := -1, -1
	for i, e := range events {
		switch e {
		case "rollout-request":
			if rolloutIdx == -1 {
				rolloutIdx = i
			}
		case "rollback-close-tunnel":
			rollbackIdx = i
		}
	}

	if rolloutIdx == -1 {
		t.Fatalf("rollout request never reached the apiserver — tunnel was closed before the rollout wait (events: %v)", events)
	}
	if rollbackIdx == -1 {
		t.Fatalf("rollback func was not executed (events: %v)", events)
	}
	if rolloutIdx > rollbackIdx {
		t.Fatalf("rollout wait ran after the tunnel was torn down (events: %v)", events)
	}
}

// TestSyncOptions_Cleanup_NilFactory_NoPanic pins the daemon-crash regression
// where the daemon (daemon/action/sync.go) registers the deferred Cleanup on the
// error path BEFORE InitClient sets the embedded factory. When an earlier step
// (e.g. resolveKubeconfig) fails, that deferred Cleanup ran with a nil factory
// and dereferenced it inside GetUnstructuredObject / DynamicClient / RolloutStatus
// -> SIGSEGV, which the panic interceptor then turned into a full daemon crash.
//
// Cleanup must instead no-op the factory-dependent work when factory is nil, while
// STILL running the rollback funcs that tear down the session (SSH tunnel + temp
// kubeconfig) — otherwise those leak on the error path.
func TestSyncOptions_Cleanup_NilFactory_NoPanic(t *testing.T) {
	// Never call InitClient: the embedded K8sClient.factory stays nil, exactly as
	// on the daemon's early-error path.
	opts := &SyncOptions{
		WorkloadNamespace:   "test-ns",
		Workloads:           []string{"deployments/authors"},
		TargetWorkloadNames: map[string]string{"authors": "authors-clone"},
	}

	rolledBack := false
	opts.AddRollbackFunc(func() error {
		rolledBack = true
		return nil
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Cleanup panicked with nil factory: %v", r)
		}
	}()

	if err := opts.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup with nil factory returned error: %v", err)
	}
	if !rolledBack {
		t.Fatal("rollback func was not executed on the nil-factory path (session teardown would leak)")
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
