package inject

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

func TestAddEnvoyConfig_NewEntry(t *testing.T) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			config.KeyEnvoy: "",
		},
	}
	clientset := fake.NewSimpleClientset(cm)
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	ports := []xds.ContainerPort{
		{ContainerPort: 8080, Protocol: "TCP"},
	}
	headers := map[string]string{"version": "v1"}
	portmap := map[int32]string{8080: "9090"}

	err := addEnvoyConfig(context.Background(), mapInterface, envoyRuleSpec{Namespace: "test-ns", NodeID: "deployments.apps.nginx", LocalTunIPv4: "10.0.0.1", LocalTunIPv6: "fd00::1", Headers: headers, Ports: ports, PortMap: portmap, OwnerID: "test-owner"})
	if err != nil {
		t.Fatalf("addEnvoyConfig returned error: %v", err)
	}

	// Verify the ConfigMap was updated
	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	data := updated.Data[config.KeyEnvoy]
	if data == "" {
		t.Fatal("expected non-empty envoy config data")
	}

	// Verify content by parsing back
	var virtuals []*xds.Virtual
	if err := yamlUnmarshal([]byte(data), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal envoy config: %v", err)
	}

	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual entry, got %d", len(virtuals))
	}
	vr := virtuals[0]
	if vr.UID != "deployments.apps.nginx" {
		t.Errorf("expected UID 'deployments.apps.nginx', got %q", vr.UID)
	}
	if vr.Namespace != "test-ns" {
		t.Errorf("expected Namespace 'test-ns', got %q", vr.Namespace)
	}
	if vr.FargateMode {
		t.Error("expected FargateMode=false")
	}
	if len(vr.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(vr.Rules))
	}
	if vr.Rules[0].LocalTunIPv4 != "10.0.0.1" {
		t.Errorf("expected LocalTunIPv4 '10.0.0.1', got %q", vr.Rules[0].LocalTunIPv4)
	}
	if vr.Rules[0].Headers["version"] != "v1" {
		t.Errorf("expected header version=v1, got %v", vr.Rules[0].Headers)
	}
}

func TestAddEnvoyConfig_MergeExisting(t *testing.T) {
	// Pre-populate with an existing entry for the same nodeID/namespace/ownerID
	initialYAML := `- uid: deployments.apps.nginx
  namespace: test-ns
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "10.0.0.1"
    localtunipv6: "fd00::1"
    ownerID: test-owner
    portmap:
      8080: "9090"
`
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			config.KeyEnvoy: initialYAML,
		},
	}
	clientset := fake.NewSimpleClientset(cm)
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	// Add with same OwnerID → should merge headers
	newHeaders := map[string]string{"env": "dev"}
	newPortmap := map[int32]string{9090: "7070"}

	err := addEnvoyConfig(context.Background(), mapInterface, envoyRuleSpec{Namespace: "test-ns", NodeID: "deployments.apps.nginx", LocalTunIPv4: "10.0.0.1", LocalTunIPv6: "fd00::1", Headers: newHeaders, PortMap: newPortmap, OwnerID: "test-owner"})
	if err != nil {
		t.Fatalf("addEnvoyConfig returned error: %v", err)
	}

	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	var virtuals []*xds.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(virtuals))
	}
	if len(virtuals[0].Rules) != 1 {
		t.Fatalf("expected 1 rule (merged), got %d", len(virtuals[0].Rules))
	}
	rule := virtuals[0].Rules[0]
	// Both headers should be present (merged)
	if rule.Headers["version"] != "v1" {
		t.Errorf("expected header version=v1, got %v", rule.Headers)
	}
	if rule.Headers["env"] != "dev" {
		t.Errorf("expected header env=dev, got %v", rule.Headers)
	}
	// Both portmap entries should be present (merged)
	if rule.PortMap[8080] != "9090" {
		t.Errorf("expected portmap 8080->9090, got %v", rule.PortMap)
	}
	if rule.PortMap[9090] != "7070" {
		t.Errorf("expected portmap 9090->7070, got %v", rule.PortMap)
	}
}

func TestAddEnvoyConfig_FargateMode(t *testing.T) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			config.KeyEnvoy: "",
		},
	}
	clientset := fake.NewSimpleClientset(cm)
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	ports := []xds.ContainerPort{
		{ContainerPort: 8080, EnvoyListenerPort: 15001, Protocol: "TCP"},
	}
	headers := map[string]string{"user": "alice"}
	portmap := map[int32]string{8080: "15001:9090"}

	err := addEnvoyConfig(context.Background(), mapInterface, envoyRuleSpec{Namespace: "test-ns", NodeID: "deployments.apps.web", LocalTunIPv4: "10.0.0.5", LocalTunIPv6: "fd00::5", Headers: headers, Ports: ports, PortMap: portmap, FargateMode: true, OwnerID: "test-owner"})
	if err != nil {
		t.Fatalf("addEnvoyConfig returned error: %v", err)
	}

	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	var virtuals []*xds.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(virtuals))
	}
	vr := virtuals[0]
	if !vr.FargateMode {
		t.Error("expected FargateMode=true")
	}
	if !vr.IsFargateMode() {
		t.Error("expected IsFargateMode() to return true")
	}
	if vr.Ports[0].EnvoyListenerPort != 15001 {
		t.Errorf("expected EnvoyListenerPort 15001, got %d", vr.Ports[0].EnvoyListenerPort)
	}
}

func TestRemoveEnvoyConfig_Found(t *testing.T) {
	// First add an entry, then remove it
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			config.KeyEnvoy: "",
		},
	}
	clientset := fake.NewSimpleClientset(cm)
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	ports := []xds.ContainerPort{
		{ContainerPort: 8080, Protocol: "TCP"},
	}
	headers := map[string]string{"version": "v2"}
	portmap := map[int32]string{8080: "9090"}

	err := addEnvoyConfig(context.Background(), mapInterface, envoyRuleSpec{Namespace: "test-ns", NodeID: "deployments.apps.nginx", LocalTunIPv4: "10.0.0.1", LocalTunIPv6: "fd00::1", Headers: headers, Ports: ports, PortMap: portmap, OwnerID: "test-owner"})
	if err != nil {
		t.Fatalf("addEnvoyConfig returned error: %v", err)
	}

	// Remove using ownerID
	empty, found, err := removeEnvoyConfig(context.Background(), mapInterface, "test-ns", "deployments.apps.nginx", "test-owner")
	if err != nil {
		t.Fatalf("removeEnvoyConfig returned error: %v", err)
	}
	if !found {
		t.Error("expected found=true")
	}
	if !empty {
		t.Error("expected empty=true after removing the only rule")
	}

	// Verify the ConfigMap has empty virtuals
	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}
	var virtuals []*xds.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(virtuals) != 0 {
		t.Errorf("expected 0 virtuals after removal, got %d", len(virtuals))
	}
}

func TestRemoveEnvoyConfig_NotFound(t *testing.T) {
	// ConfigMap exists but has no matching entry → found=false
	initialYAML := `- uid: deployments.apps.nginx
  namespace: test-ns
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "10.0.0.1"
    localtunipv6: "fd00::1"
    portmap:
      8080: "9090"
`
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: "test-ns",
		},
		Data: map[string]string{
			config.KeyEnvoy: initialYAML,
		},
	}
	clientset := fake.NewSimpleClientset(cm)
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	// Try to remove a non-existent nodeID
	empty, found, err := removeEnvoyConfig(context.Background(), mapInterface, "test-ns", "deployments.apps.nonexistent", "any-owner")
	if err != nil {
		t.Fatalf("removeEnvoyConfig returned error: %v", err)
	}
	if found {
		t.Error("expected found=false for non-existent nodeID")
	}
	if empty {
		t.Error("expected empty=false when nothing was removed")
	}
}

func TestRemoveEnvoyConfig_ConfigMapNotFound(t *testing.T) {
	// No ConfigMap exists at all → empty=true, found=false, no error
	clientset := fake.NewSimpleClientset()
	mapInterface := clientset.CoreV1().ConfigMaps("test-ns")

	empty, found, err := removeEnvoyConfig(context.Background(), mapInterface, "test-ns", "deployments.apps.nginx", "any-owner")
	if err != nil {
		t.Fatalf("removeEnvoyConfig returned error: %v", err)
	}
	if !empty {
		t.Error("expected empty=true when ConfigMap not found")
	}
	if found {
		t.Error("expected found=false when ConfigMap not found")
	}
}

// yamlUnmarshal is a test helper wrapping sigs.k8s.io/yaml.Unmarshal
func yamlUnmarshal(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
