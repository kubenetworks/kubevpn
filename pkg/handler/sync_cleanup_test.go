//go:build !integration

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

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

	// Simulate the production rollback func: tearing down the SSH tunnel by
	// closing the apiserver endpoint. This MUST run after the rollout wait.
	opts.AddRollbackFunc(func() error {
		record("rollback-close-tunnel")
		closeSrv()
		return nil
	})

	// No delete targets — the test is purely about the rollout-vs-rollback order.
	_ = opts.Cleanup(context.Background())

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

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
