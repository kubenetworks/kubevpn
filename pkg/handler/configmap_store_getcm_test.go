package handler

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestConfigMapStore_GetConfigMap_GETFallback(t *testing.T) {
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

	// Informer cache is cold on first call → GET fallback returns the ConfigMap.
	got, err := store.GetConfigMap(context.Background())
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if got == nil || got.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected traffic manager ConfigMap, got %v", got)
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
