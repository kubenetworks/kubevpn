package handler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func newTestMapper(ns, workload string, headers map[string]string) *Mapper {
	return &Mapper{
		ns:       ns,
		workload: workload,
		headers:  headers,
	}
}

func mustMarshalVirtuals(t *testing.T, virtuals []*controlplane.Virtual) string {
	t.Helper()
	b, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatalf("failed to marshal virtuals: %v", err)
	}
	return string(b)
}

func TestExtractPortMapping_EmptyConfigMapData(t *testing.T) {
	m := newTestMapper("default", "deployments.apps/nginx", nil)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm"},
	}

	result, err := m.extractPortMapping(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestExtractPortMapping_NoMatchingWorkload(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{
			Namespace: "default",
			UID:       "deployments.apps.other-app",
			Rules: []*controlplane.Rule{
				{
					Headers: map[string]string{"x-user": "alice"},
					PortMap: map[int32]string{80: "9080:8080"},
				},
			},
		},
	}

	m := newTestMapper("default", "deployments.apps/nginx", map[string]string{"x-user": "alice"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm"},
		Data: map[string]string{
			config.KeyEnvoy: mustMarshalVirtuals(t, virtuals),
		},
	}

	result, err := m.extractPortMapping(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestExtractPortMapping_MatchingWorkloadWithPortMapping(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{
			Namespace: "test-ns",
			UID:       "deployments.apps.productpage",
			Rules: []*controlplane.Rule{
				{
					Headers: map[string]string{"x-user": "dev"},
					PortMap: map[int32]string{
						80:  "9080:8080",
						443: "9443:8443",
					},
				},
			},
		},
	}

	m := newTestMapper("test-ns", "deployments.apps/productpage", map[string]string{"x-user": "dev"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm"},
		Data: map[string]string{
			config.KeyEnvoy: mustMarshalVirtuals(t, virtuals),
		},
	}

	result, err := m.extractPortMapping(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// PortMap "9080:8080" => envoyRulePort=9080, localPort=8080
	// PortMap "9443:8443" => envoyRulePort=9443, localPort=8443
	// result maps localPort -> envoyRulePort
	expected := map[int32]int32{
		8080: 9080,
		8443: 9443,
	}
	if len(result) != len(expected) {
		t.Fatalf("expected %d entries, got %d: %v", len(expected), len(result), result)
	}
	for k, v := range expected {
		if result[k] != v {
			t.Errorf("result[%d] = %d, want %d", k, result[k], v)
		}
	}
}

func TestExtractPortMapping_MatchingWorkloadDifferentHeaders(t *testing.T) {
	virtuals := []*controlplane.Virtual{
		{
			Namespace: "default",
			UID:       "deployments.apps.nginx",
			Rules: []*controlplane.Rule{
				{
					Headers: map[string]string{"x-env": "staging"},
					PortMap: map[int32]string{80: "9080:8080"},
				},
			},
		},
	}

	// mapper headers do not match rule headers
	m := newTestMapper("default", "deployments.apps/nginx", map[string]string{"x-env": "production"})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm"},
		Data: map[string]string{
			config.KeyEnvoy: mustMarshalVirtuals(t, virtuals),
		},
	}

	result, err := m.extractPortMapping(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map for mismatched headers, got %v", result)
	}
}

func TestExtractPortMapping_InvalidYAML(t *testing.T) {
	m := newTestMapper("default", "deployments.apps/nginx", nil)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm"},
		Data: map[string]string{
			config.KeyEnvoy: "{{invalid yaml::",
		},
	}

	_, err := m.extractPortMapping(cm)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}
