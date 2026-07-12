package handler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// A cold cache must NEVER issue a live-API GET (the removed fallback made every daemon
// read block on the TCP timeout when the cluster was unreachable). Install a reactor on the
// "get" verb that records/fails if hit, and assert GetConfigMap on a cold cache returns
// (nil, nil) without touching it. After EnsureSynced, the read is served from the cache.
func TestConfigMapStore_GetConfigMap_CacheOnly_NoLiveGET(t *testing.T) {
	const ns = "test"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string]string{config.KeyEnvoy: "[]"},
	}
	clientset := fake.NewSimpleClientset(cm)
	var liveGetCalled atomic.Bool
	clientset.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		liveGetCalled.Store(true)
		return true, nil, fmt.Errorf("live GET must not be called from the read path")
	})
	store := &ConfigMapStore{clientset: clientset, managerNamespace: ns}
	defer store.Stop()

	// Cold cache → (nil, nil), no error, and no live GET.
	got, err := store.GetConfigMap(context.Background())
	if err != nil {
		t.Fatalf("GetConfigMap on cold cache: unexpected error %v", err)
	}
	if got != nil {
		t.Fatalf("cold cache should miss (nil), got %v", got)
	}
	if liveGetCalled.Load() {
		t.Fatal("read path issued a live-API GET — fallback was not removed")
	}

	// After warm-up the read is served from the informer cache (still no live GET).
	if err := store.EnsureSynced(context.Background()); err != nil {
		t.Fatalf("EnsureSynced: %v", err)
	}
	got, err = store.GetConfigMap(context.Background())
	if err != nil {
		t.Fatalf("GetConfigMap after sync: %v", err)
	}
	if got == nil || got.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected traffic manager ConfigMap from cache, got %v", got)
	}
	if liveGetCalled.Load() {
		t.Fatal("warm read path issued a live-API GET")
	}
}

// EnsureSynced must return a bounded error (not block indefinitely) when the informer
// cannot sync — e.g. the cluster is unreachable and List keeps failing.
func TestConfigMapStore_EnsureSynced_TimesOut(t *testing.T) {
	const ns = "test"
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("cluster unreachable")
	})
	store := &ConfigMapStore{clientset: clientset, managerNamespace: ns}
	defer store.Stop()

	// Shorten the bound so the test is fast; restore afterwards.
	orig := config.ConfigMapSyncTimeout
	config.ConfigMapSyncTimeout = 200 * time.Millisecond
	defer func() { config.ConfigMapSyncTimeout = orig }()

	start := time.Now()
	err := store.EnsureSynced(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error when informer cannot sync, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("EnsureSynced did not respect the bound: took %v", elapsed)
	}
}

// Regression: a retried Cleanup calls Stop() more than once; Stop must be
// idempotent and never close the informer stop channel twice.
func TestConfigMapStore_Stop_Idempotent(t *testing.T) {
	t.Run("before informer created", func(t *testing.T) {
		store := &ConfigMapStore{
			clientset:        fake.NewSimpleClientset(),
			managerNamespace: "test",
		}
		store.Stop()
		store.Stop() // must not panic
	})

	t.Run("after informer created", func(t *testing.T) {
		store := &ConfigMapStore{
			clientset:        fake.NewSimpleClientset(),
			managerNamespace: "test",
		}
		store.GetInformer() // creates informerStop and starts the informer goroutine
		store.Stop()
		store.Stop() // second close of informerStop would panic without the fix
	})
}

func TestConfigMapStore_GetConfigMap_FromInformerCache(t *testing.T) {
	const ns = "test"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string]string{config.KeyEnvoy: "[]"},
	}
	store := &ConfigMapStore{
		clientset:        fake.NewSimpleClientset(cm),
		managerNamespace: ns,
	}
	defer store.Stop()

	informer := store.GetInformer()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("informer did not sync")
	}

	got, err := store.GetConfigMap(context.Background())
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if got == nil || got.Data[config.KeyEnvoy] != "[]" {
		t.Fatalf("expected ConfigMap from informer cache, got %v", got)
	}
}
