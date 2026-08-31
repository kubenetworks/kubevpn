package xds

// Integration tests for lease-renewal persistence and the restart path that consumes it.
//
// Why these exist: renewals were written to TUN_ALLOCS only by WatchTunIP's LeaseDuration/3 ticker.
// A client whose port-forward was being torn down every 30s never kept a stream alive for the 100s
// that takes, so its persisted lastRenew froze while its in-memory lease stayed perfectly fresh —
// exactly what was found in the field: a live client with a 1h54m-old timestamp in the ConfigMap.
// The reaper was right not to reclaim it; the ConfigMap was lying. The danger is the restart path:
// loadAllocs reads that frozen timestamp, declares the live client expired, releases its IP, and the
// next client can be handed the same address.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// persistedAllocsOf reads TUN_ALLOCS straight out of the ConfigMap — the state a restarting traffic
// manager would see, as opposed to the server's in-memory view.
func persistedAllocsOf(t *testing.T, clientset kubernetes.Interface, ns string) map[string]*persistedAlloc {
	t.Helper()
	cm, err := clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	got, err := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
	if err != nil {
		t.Fatalf("parse %s: %v", config.KeyTunAllocs, err)
	}
	return got
}

// freezePersistedLastRenew rewrites one owner's persisted lastRenew to age ago, reproducing the
// frozen timestamp without waiting for it.
func freezePersistedLastRenew(t *testing.T, s *TunConfigServer, owner string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	persisted, err := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
	if err != nil {
		t.Fatalf("parse allocs: %v", err)
	}
	pa, ok := persisted[owner]
	if !ok {
		t.Fatalf("owner %q not present in persisted allocs %v", owner, persisted)
	}
	pa.LastRenew = time.Now().Add(-age).Unix()
	writePersistedAllocs(t, s, persisted)
}

// writePersistedAllocs writes allocs back to TUN_ALLOCS through the same yaml encoding saveAllocs
// uses, so the on-disk shape is identical to what the server itself would produce.
func writePersistedAllocs(t *testing.T, s *TunConfigServer, persisted map[string]*persistedAlloc) {
	t.Helper()
	ctx := context.Background()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	data, err := yaml.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal allocs: %v", err)
	}
	cm.Data[config.KeyTunAllocs] = string(data)
	if _, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update configmap: %v", err)
	}
}

// mockWatchStream drives a real WatchTunIP subscription whose lifetime the test controls. Only Send
// and Context are exercised; the embedded interface satisfies the rest of grpc.ServerStream.
type mockWatchStream struct {
	grpc.ServerStream
	ctx context.Context

	mu   sync.Mutex
	sent []*rpc.TunIPResponse
}

func (m *mockWatchStream) Send(resp *rpc.TunIPResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, resp)
	return nil
}

func (m *mockWatchStream) Context() context.Context { return m.ctx }

// waitForWatcher blocks until the server has registered want subscribers for owner, so a test never
// cancels a stream before WatchTunIP has done its subscribe-time renewal.
func waitForWatcher(t *testing.T, s *TunConfigServer, owner string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.WatcherCount(owner) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("owner %q never reached %d watcher(s); it has %d", owner, want, s.WatcherCount(owner))
}

// TestIntegration_LeaseRenewalPersistsDespiteFlappingStream reproduces the frozen timestamp: a
// client whose xDS stream never survives long enough to reach WatchTunIP's own persist ticker must
// still have its renewals written down.
func TestIntegration_LeaseRenewalPersistsDespiteFlappingStream(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	const owner = "flapping-client"

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns", Hostname: "naison-macmini.local"})
	if err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}
	firstPersisted := persistedAllocsOf(t, s.clientset, s.namespace)[owner]
	if firstPersisted == nil {
		t.Fatal("allocation was not persisted at all")
	}

	// lastRenew has one-second granularity, so let it move on before renewing.
	time.Sleep(1100 * time.Millisecond)

	// The production loop: a stream is established, renews on subscribe, and dies well before
	// LeaseDuration/3. Repeat it a few times — none of these can reach the stream's own ticker.
	for i := 0; i < 3; i++ {
		streamCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- s.WatchTunIP(&rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns"}, &mockWatchStream{ctx: streamCtx})
		}()
		waitForWatcher(t, s, owner, 1)
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("WatchTunIP did not return after its stream context was cancelled")
		}
	}

	// In-memory state is fresh — the reaper is right not to reclaim.
	s.mu.RLock()
	inMemory := s.allocs[owner].LastRenew
	s.mu.RUnlock()
	if !inMemory.After(time.Unix(firstPersisted.LastRenew, 0)) {
		t.Fatalf("in-memory lease was not renewed by the streams: %v", inMemory)
	}

	// Nothing has flushed yet, so the ConfigMap is still behind: that is the bug's mechanism.
	// The reaper's tick is what must fix it, without any stream living long enough to help.
	s.reapExpiredLeases(ctx)

	after := persistedAllocsOf(t, s.clientset, s.namespace)[owner]
	if after == nil {
		t.Fatal("allocation disappeared from TUN_ALLOCS")
	}
	if after.LastRenew <= firstPersisted.LastRenew {
		t.Fatalf("persisted lastRenew did not advance (%d -> %d); a client whose stream cannot survive "+
			"LeaseDuration/3 must still get its renewal written down",
			firstPersisted.LastRenew, after.LastRenew)
	}
	if after.IPv4 != resp.IPv4 {
		t.Fatalf("persisted IPv4 changed: %q -> %q", resp.IPv4, after.IPv4)
	}
	t.Logf("✅ persisted lastRenew advanced %d -> %d without any long-lived stream",
		firstPersisted.LastRenew, after.LastRenew)
}

// TestIntegration_RestartKeepsLiveClientIP is the consequence that made the frozen timestamp
// dangerous rather than merely untidy: a restart must not hand a live client's address to someone
// else on the strength of a stale record.
func TestIntegration_RestartKeepsLiveClientIP(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	const live = "live-client"

	resp, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: live, Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}
	liveIP, _, err := net.ParseCIDR(resp.IPv4)
	if err != nil {
		t.Fatalf("parse allocated IPv4 %q: %v", resp.IPv4, err)
	}

	// Persisted lastRenew is past LeaseDuration but still inside the grace window, while the client
	// is very much alive. This is the residual window the grace exists for: the flush runs every
	// leaseReapInterval, so a live client's record can legitimately be that far behind, and a
	// restart lands with the client's stream broken and not yet re-subscribed.
	//
	// The grace deliberately does NOT stretch to cover the 1h54m-stale record seen in the field —
	// a record that old is indistinguishable from a dead client's, and the freeze that produced it
	// is fixed at the source by persisting renewals on the reaper's tick.
	freezePersistedLastRenew(t, s, live, LeaseDuration+30*time.Second)

	// Restart the traffic manager against the same ConfigMap.
	restarted, err := NewTunConfigServer(ctx, s.clientset, s.namespace)
	if err != nil {
		t.Fatalf("restart NewTunConfigServer: %v", err)
	}

	restarted.mu.RLock()
	_, kept := restarted.allocs[live]
	restarted.mu.RUnlock()
	if !kept {
		t.Fatal("restart dropped a live client's allocation based on a stale persisted timestamp")
	}

	// The address must not be handed out again. Ask for several so a lucky pick cannot hide a leak.
	for i := 0; i < 5; i++ {
		other := fmt.Sprintf("other-client-%d", i)
		otherResp, err := restarted.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: other, Namespace: "test-ns"})
		if err != nil {
			t.Fatalf("GetTunIP for %s: %v", other, err)
		}
		otherIP, _, err := net.ParseCIDR(otherResp.IPv4)
		if err != nil {
			t.Fatalf("parse %q: %v", otherResp.IPv4, err)
		}
		if otherIP.Equal(liveIP) {
			t.Fatalf("%s was handed %s, which %s is still using", other, otherIP, live)
		}
	}
	t.Logf("✅ restart kept %s for %s and handed it to nobody else", liveIP, live)
}

// TestIntegration_RestartStillReclaimsTrulyGoneClient pins the other side of the grace window: it
// buys a live client time, it does not leak addresses forever.
func TestIntegration_RestartStillReclaimsTrulyGoneClient(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	const gone = "gone-client"

	if _, err := s.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: gone, Namespace: "test-ns"}); err != nil {
		t.Fatalf("GetTunIP: %v", err)
	}
	// Past LeaseDuration + loadGrace: no live client can be this far behind.
	freezePersistedLastRenew(t, s, gone, LeaseDuration+loadGrace+time.Minute)

	restarted, err := NewTunConfigServer(ctx, s.clientset, s.namespace)
	if err != nil {
		t.Fatalf("restart NewTunConfigServer: %v", err)
	}
	restarted.mu.RLock()
	_, kept := restarted.allocs[gone]
	restarted.mu.RUnlock()
	if kept {
		t.Fatalf("an allocation stale by more than LeaseDuration+loadGrace (%v) must be reclaimed on restart",
			LeaseDuration+loadGrace)
	}
	t.Log("✅ restart still reclaims an allocation that is stale beyond the grace window")
}
