package inject

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func fakeSecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-tls-secret", Namespace: "default"},
		Data: map[string][]byte{
			config.TLSServerName:    []byte("test-server"),
			config.TLSCertKey:       []byte("fake-cert-data"),
			config.TLSPrivateKeyKey: []byte("fake-key-data"),
		},
	}
}

func basePodTemplateSpec() *v1.PodTemplateSpec {
	return &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app", Image: "nginx:latest"},
			},
		},
	}
}

// Req3: the injected fargate VPN sidecar (server) defaults to --debug.
func TestAddEnvoyAndSSHContainer_DefaultDebug(t *testing.T) {
	spec := basePodTemplateSpec()
	AddEnvoyAndSSHContainer(spec, "default", "node1", false, "kubevpn-system", "kubevpn:latest")

	var found bool
	for _, c := range spec.Spec.Containers {
		if c.Name != config.ContainerSidecarVPN {
			continue
		}
		found = true
		var hasDebug bool
		for _, a := range c.Args {
			if a == "--debug" {
				hasDebug = true
			}
		}
		if !hasDebug {
			t.Errorf("vpn sidecar args should contain --debug, got %v", c.Args)
		}
	}
	if !found {
		t.Fatalf("vpn sidecar container %q not found", config.ContainerSidecarVPN)
	}
}

// Regression: mesh injection must NOT pin the workload pod to the traffic-manager
// ServiceAccount. That SA only exists in the manager namespace; referencing it from a
// workload in another namespace makes the ServiceAccount admission controller reject
// pod creation, so the new ReplicaSet never produces a pod, the rollout times out, and
// RolloutStatus undoes the patch — leaving the workload on its original single-container
// spec. The sidecar reaches the control plane over gRPC+TLS and needs no SA token.
func TestAddVPNAndEnvoyContainer_DoesNotPinManagerServiceAccount(t *testing.T) {
	spec := basePodTemplateSpec()
	// Workload lives in "default"; manager is in "kubevpn-system" — a different namespace.
	AddVPNAndEnvoyContainer(spec, "default", "node1", false, "kubevpn-system", fakeSecret(), "kubevpn:test")

	if spec.Spec.ServiceAccountName == config.ConfigMapPodTrafficManager {
		t.Errorf("injected pod must not reference the manager ServiceAccount %q "+
			"(it does not exist in the workload namespace and blocks pod creation)",
			config.ConfigMapPodTrafficManager)
	}
}

func TestAddVPNAndEnvoyContainer(t *testing.T) {
	spec := basePodTemplateSpec()
	secret := fakeSecret()
	image := "docker.io/naison/kubevpn:test"

	AddVPNAndEnvoyContainer(spec, "default", "node1", false, "kubevpn-system", secret, image)

	// Should have 3 containers: original app + vpn + envoy
	if len(spec.Spec.Containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(spec.Spec.Containers))
	}

	var vpnContainer, envoyContainer *v1.Container
	for i := range spec.Spec.Containers {
		switch spec.Spec.Containers[i].Name {
		case config.ContainerSidecarVPN:
			vpnContainer = &spec.Spec.Containers[i]
		case config.ContainerSidecarEnvoy:
			envoyContainer = &spec.Spec.Containers[i]
		}
	}

	if vpnContainer == nil {
		t.Fatal("vpn container not found")
	}
	if envoyContainer == nil {
		t.Fatal("envoy container not found")
	}

	// VPN container checks
	if vpnContainer.Image != image {
		t.Errorf("vpn container image: got %q, want %q", vpnContainer.Image, image)
	}
	if vpnContainer.SecurityContext == nil {
		t.Fatal("vpn container SecurityContext is nil")
	}
	// VPN container writes /proc/sys/net/ipv4/ip_forward at startup, which
	// requires a privileged container.
	if vpnContainer.SecurityContext.Privileged == nil || !*vpnContainer.SecurityContext.Privileged {
		t.Error("vpn container should be privileged to write /proc/sys/net/ipv4/ip_forward")
	}
	if vpnContainer.SecurityContext.RunAsUser == nil || *vpnContainer.SecurityContext.RunAsUser != 0 {
		t.Error("vpn container should run as root (uid 0)")
	}
	if vpnContainer.SecurityContext.Capabilities == nil {
		t.Fatal("vpn container Capabilities is nil")
	}
	requiredCaps := map[v1.Capability]bool{"NET_ADMIN": false, "NET_RAW": false}
	for _, cap := range vpnContainer.SecurityContext.Capabilities.Add {
		if _, ok := requiredCaps[cap]; ok {
			requiredCaps[cap] = true
		}
	}
	for cap, found := range requiredCaps {
		if !found {
			t.Errorf("vpn container should have %s capability", cap)
		}
	}

	// VPN container startup script uses bash-specific syntax (${b/iptables/ip6tables}
	// parameter substitution, &> redirect), so it must run under bash, not sh/dash.
	if len(vpnContainer.Command) == 0 || vpnContainer.Command[0] != "/bin/bash" {
		t.Errorf("vpn container should run under /bin/bash (script uses bash syntax), got command %v", vpnContainer.Command)
	}

	// VPN container should have iptables commands in args
	if len(vpnContainer.Args) == 0 {
		t.Fatal("vpn container args should not be empty")
	}
	if !strings.Contains(vpnContainer.Args[0], "iptables") {
		t.Error("vpn container args should contain iptables commands")
	}

	// VPN container should have TLS env vars
	envMap := make(map[string]string)
	for _, e := range vpnContainer.Env {
		envMap[e.Name] = e.Value
	}
	if envMap[config.TLSServerName] != "test-server" {
		t.Errorf("expected TLSServerName env 'test-server', got %q", envMap[config.TLSServerName])
	}
	if envMap[config.TLSCertKey] != "fake-cert-data" {
		t.Errorf("expected TLSCertKey env 'fake-cert-data', got %q", envMap[config.TLSCertKey])
	}
	if envMap[config.TLSPrivateKeyKey] != "fake-key-data" {
		t.Errorf("expected TLSPrivateKeyKey env 'fake-key-data', got %q", envMap[config.TLSPrivateKeyKey])
	}

	// Envoy container checks
	if envoyContainer.Image != image {
		t.Errorf("envoy container image: got %q, want %q", envoyContainer.Image, image)
	}
	if envoyContainer.Command[0] != "envoy" {
		t.Errorf("envoy container command should start with 'envoy', got %q", envoyContainer.Command[0])
	}
}

func TestAddVPNAndEnvoyContainer_Idempotent(t *testing.T) {
	spec := basePodTemplateSpec()
	secret := fakeSecret()
	image := "docker.io/naison/kubevpn:test"

	// Call twice — should not duplicate sidecar containers
	AddVPNAndEnvoyContainer(spec, "default", "node1", false, "kubevpn-system", secret, image)
	AddVPNAndEnvoyContainer(spec, "default", "node1", false, "kubevpn-system", secret, image)

	vpnCount := 0
	envoyCount := 0
	for _, c := range spec.Spec.Containers {
		if c.Name == config.ContainerSidecarVPN {
			vpnCount++
		}
		if c.Name == config.ContainerSidecarEnvoy {
			envoyCount++
		}
	}
	if vpnCount != 1 {
		t.Errorf("expected 1 vpn container after double-add, got %d", vpnCount)
	}
	if envoyCount != 1 {
		t.Errorf("expected 1 envoy container after double-add, got %d", envoyCount)
	}
}

func TestAddEnvoyAndSSHContainer(t *testing.T) {
	spec := basePodTemplateSpec()
	image := "docker.io/naison/kubevpn:test"

	AddEnvoyAndSSHContainer(spec, "default", "node1", false, "kubevpn-system", image)

	// Should have 3 containers: original app + vpn + envoy
	if len(spec.Spec.Containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(spec.Spec.Containers))
	}

	var vpnContainer, envoyContainer *v1.Container
	for i := range spec.Spec.Containers {
		switch spec.Spec.Containers[i].Name {
		case config.ContainerSidecarVPN:
			vpnContainer = &spec.Spec.Containers[i]
		case config.ContainerSidecarEnvoy:
			envoyContainer = &spec.Spec.Containers[i]
		}
	}

	if vpnContainer == nil {
		t.Fatal("vpn container not found")
	}
	if envoyContainer == nil {
		t.Fatal("envoy container not found")
	}

	// VPN container runs SSH server
	if vpnContainer.Image != image {
		t.Errorf("vpn container image: got %q, want %q", vpnContainer.Image, image)
	}
	argsJoined := strings.Join(vpnContainer.Args, " ")
	if !strings.Contains(argsJoined, "ssh://") {
		t.Errorf("vpn container args should contain SSH server listen address, got %v", vpnContainer.Args)
	}
	if vpnContainer.Command[0] != "kubevpn" {
		t.Errorf("vpn container command should be 'kubevpn', got %q", vpnContainer.Command[0])
	}

	// SecurityContext should be non-nil but NOT privileged (fargate mode)
	if vpnContainer.SecurityContext == nil {
		t.Fatal("vpn container SecurityContext should not be nil")
	}
	if vpnContainer.SecurityContext.Privileged != nil && *vpnContainer.SecurityContext.Privileged {
		t.Error("vpn container in fargate mode should NOT be privileged")
	}

	// Envoy container checks
	if envoyContainer.Image != image {
		t.Errorf("envoy container image: got %q, want %q", envoyContainer.Image, image)
	}
}

func TestRemoveContainers_EmptySpec(t *testing.T) {
	spec := &v1.PodSpec{
		Containers: []v1.Container{},
	}

	// Should not panic on empty containers
	RemoveContainers(spec)

	if len(spec.Containers) != 0 {
		t.Errorf("expected 0 containers after removal from empty spec, got %d", len(spec.Containers))
	}
}

func TestRemoveContainers_OnlySidecars(t *testing.T) {
	spec := &v1.PodSpec{
		Containers: []v1.Container{
			{Name: config.ContainerSidecarVPN, Image: "vpn-image"},
			{Name: config.ContainerSidecarEnvoy, Image: "envoy-image"},
		},
	}

	RemoveContainers(spec)

	if len(spec.Containers) != 0 {
		t.Errorf("expected 0 containers after removing only-sidecar spec, got %d", len(spec.Containers))
	}
}

func TestRemoveContainers_PreservesNonSidecars(t *testing.T) {
	spec := &v1.PodSpec{
		Containers: []v1.Container{
			{Name: "app", Image: "nginx:latest"},
			{Name: config.ContainerSidecarVPN, Image: "vpn-image"},
			{Name: "worker", Image: "worker:latest"},
			{Name: config.ContainerSidecarEnvoy, Image: "envoy-image"},
		},
	}

	RemoveContainers(spec)

	if len(spec.Containers) != 2 {
		t.Fatalf("expected 2 containers after removal, got %d", len(spec.Containers))
	}
	if spec.Containers[0].Name != "app" {
		t.Errorf("expected first container 'app', got %q", spec.Containers[0].Name)
	}
	if spec.Containers[1].Name != "worker" {
		t.Errorf("expected second container 'worker', got %q", spec.Containers[1].Name)
	}
}

func TestRemoveContainers_NilContainers(t *testing.T) {
	spec := &v1.PodSpec{
		Containers: nil,
	}

	// Should not panic on nil containers
	RemoveContainers(spec)

	if spec.Containers != nil {
		t.Errorf("expected nil containers to remain nil, got %v", spec.Containers)
	}
}
