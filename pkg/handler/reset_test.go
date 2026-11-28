package handler

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestReset_NilReceiver(t *testing.T) {
	var c *ConnectOptions
	err := c.Reset(context.Background(), "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error for nil receiver, got: %v", err)
	}
}

func TestReset_NilClientset(t *testing.T) {
	c := &ConnectOptions{}
	err := c.Reset(context.Background(), "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error for nil clientset, got: %v", err)
	}
}

func TestReset_EmptyWorkloads(t *testing.T) {
	c := newTestConnectOptions(t)
	err := c.Reset(context.Background(), "test-ns", []string{})
	if err != nil {
		t.Fatalf("expected nil error for empty workloads, got: %v", err)
	}
}

func TestResetConfigMap_NotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error when configmap not found, got: %v", err)
	}
}

func TestResetConfigMap_NilData(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error for nil data, got: %v", err)
	}
}

func TestResetConfigMap_EmptyEnvoyKey(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: ""},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error for empty envoy key, got: %v", err)
	}
}

func TestResetConfigMap_RemoveMatchingWorkload(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{
			Namespace: "default",
			UID:       "deployments.apps.nginx",
			Rules: []*controlplane.Rule{
				{LocalTunIPv4: "10.0.0.1", IsAgent: true},
			},
		},
		{
			Namespace: "default",
			UID:       "deployments.apps.redis",
			Rules: []*controlplane.Rule{
				{LocalTunIPv4: "10.0.0.2", IsAgent: true},
			},
		},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining virtual, got %d", len(remaining))
	}
	if remaining[0].UID != "deployments.apps.redis" {
		t.Fatalf("expected remaining UID to be deployments.apps.redis, got %s", remaining[0].UID)
	}
}

func TestResetConfigMap_RejectsLegacyVirtual(t *testing.T) {
	virtuals := []*controlplane.Virtual{{
		Namespace: "default", UID: "deployments.apps.nginx",
		Rules: []*controlplane.Rule{{LocalTunIPv4: "10.0.0.1", OwnerID: "owner-a"}},
	}}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	if err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"}); err == nil {
		t.Fatal("expected legacy sidecar Virtual to be rejected")
	}
	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[config.KeyEnvoy] != string(data) {
		t.Fatal("legacy Virtual was modified during rejected reset")
	}
}

func TestResetConfigMap_RejectsMixedStateWithoutMutation(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "default", UID: "deployments.apps.nginx"},
		{
			Namespace: "default", UID: "deployments.apps.nginx.nodes.node-a",
			AgentOrigins: []*controlplane.AgentOrigin{{PodUID: "pod-a"}},
		},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	err = resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if !errors.Is(err, controlplane.ErrMixedVirtualState) {
		t.Fatalf("expected mixed-state rejection, got %v", err)
	}
	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[config.KeyEnvoy] != string(data) {
		t.Fatal("mixed state was modified during rejected reset")
	}
}

func TestResetConfigMap_RetriesConflictWithLatestState(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "default", UID: "deployments.apps.nginx"},
		{Namespace: "default", UID: "deployments.apps.redis"},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	var attempts int32
	clientset.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&attempts, 1) != 1 {
			return false, nil, nil
		}
		current, err := clientset.Tracker().Get(gvr, "test-ns", config.ConfigMapPodTrafficManager)
		if err != nil {
			return true, nil, err
		}
		latest := current.(*corev1.ConfigMap).DeepCopy()
		latest.Data[config.KeyNodeAttachments] = "concurrent-update"
		if err := clientset.Tracker().Update(gvr, latest, "test-ns"); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, config.ConfigMapPodTrafficManager, fmt.Errorf("injected conflict"))
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	if err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("reset performed %d updates, want 2", got)
	}
	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data[config.KeyNodeAttachments] != "concurrent-update" {
		t.Fatal("retry overwrote the concurrent ConfigMap update")
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].UID != "deployments.apps.redis" {
		t.Fatalf("reset did not converge to latest desired state: %+v", remaining)
	}
}

func TestResetConfigMap_NoMatchingWorkload(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{
			Namespace: "default",
			UID:       "deployments.apps.nginx",
		},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/redis"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining virtual, got %d", len(remaining))
	}
}

func TestResetConfigMap_DifferentNamespace(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "production", UID: "deployments.apps.nginx"},
		{Namespace: "default", UID: "deployments.apps.nginx"},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining virtual, got %d", len(remaining))
	}
	if remaining[0].Namespace != "production" {
		t.Fatalf("expected remaining namespace to be production, got %s", remaining[0].Namespace)
	}
}

func TestResetConfigMap_EmptyWorkloads(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "default", UID: "deployments.apps.nginx"},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining virtual (none removed), got %d", len(remaining))
	}
}

func TestResetConfigMap_RemoveMultipleWorkloads(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "default", UID: "deployments.apps.nginx"},
		{Namespace: "default", UID: "deployments.apps.redis"},
		{Namespace: "default", UID: "statefulsets.apps.postgres"},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{
		"deployments.apps/nginx",
		"statefulsets.apps/postgres",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining virtual, got %d", len(remaining))
	}
	if remaining[0].UID != "deployments.apps.redis" {
		t.Fatalf("expected remaining UID to be deployments.apps.redis, got %s", remaining[0].UID)
	}
}

func TestResetConfigMap_RemoveAllVirtuals(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{Namespace: "default", UID: "deployments.apps.nginx"},
	}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data)},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err = resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cm, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get updated configmap: %v", err)
	}
	var remaining []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &remaining); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining virtuals, got %d", len(remaining))
	}
}

func TestResetConfigMap_InvalidYAML(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: "not-valid-yaml: ["},
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")
	err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err != nil {
		t.Fatalf("expected nil error for invalid yaml (function returns nil), got: %v", err)
	}
}

func TestResetConfigMap_GetError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	err := resetConfigMap(context.Background(), mapInterface, "default", []string{"deployments.apps/nginx"})
	if err == nil {
		t.Fatal("expected error when get fails, got nil")
	}
	if err.Error() != "connection refused" {
		t.Fatalf("expected 'connection refused' error, got: %v", err)
	}
}
