package inject

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestAlreadyInjected_VPNOnly(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
				{Name: config.ContainerSidecarVPN},
			},
		},
	}
	if alreadyInjected(templateSpec) {
		t.Error("expected alreadyInjected to return false when only VPN sidecar is present (no envoy)")
	}
}

func TestAlreadyInjected_EnvoyOnly(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
				{Name: config.ContainerSidecarEnvoy},
			},
		},
	}
	if alreadyInjected(templateSpec) {
		t.Error("expected alreadyInjected to return false when only envoy sidecar is present (no VPN)")
	}
}

// TestInjectedForManager_NamespaceMigration_Mesh reproduces the traffic-manager
// namespace-migration bug: a mesh-injected workload whose envoy xds_cluster was
// baked against a now-removed manager namespace must be re-injected, not skipped.
func TestInjectedForManager_NamespaceMigration_Mesh(t *testing.T) {
	const (
		workloadNS = "default"
		nodeID     = "default.deployments.authors"
	)
	secret := &v1.Secret{Data: map[string][]byte{}}
	spec := &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{Name: "authors"}}}}

	// Inject while the traffic-manager lives in the "default" namespace.
	AddVPNAndEnvoyContainer(spec, workloadNS, nodeID, false, "default", secret, "img:test")
	if !alreadyInjected(spec) {
		t.Fatal("expected sidecars to be present after injection")
	}
	if !injectedForManager(spec, "default") {
		t.Fatal("expected injectedForManager(default)=true right after injecting against default")
	}
	// Manager moved to the "kubevpn" namespace: the baked envoy xds address is now
	// stale, so the workload must NOT be considered injected-for-this-manager.
	if injectedForManager(spec, "kubevpn") {
		t.Fatal("expected injectedForManager(kubevpn)=false: envoy still targets kubevpn-traffic-manager.default")
	}

	// Re-inject against the new manager namespace: now it matches and the stale
	// sidecars are replaced (no duplicates).
	AddVPNAndEnvoyContainer(spec, workloadNS, nodeID, false, "kubevpn", secret, "img:test")
	if !injectedForManager(spec, "kubevpn") {
		t.Fatal("expected injectedForManager(kubevpn)=true after re-injecting against kubevpn")
	}
	if injectedForManager(spec, "default") {
		t.Fatal("expected injectedForManager(default)=false after re-injection")
	}
	if n := countContainer(spec, config.ContainerSidecarVPN); n != 1 {
		t.Fatalf("expected exactly 1 vpn sidecar after re-injection, got %d", n)
	}
	if n := countContainer(spec, config.ContainerSidecarEnvoy); n != 1 {
		t.Fatalf("expected exactly 1 envoy sidecar after re-injection, got %d", n)
	}
}

// TestInjectedForManager_NamespaceMigration_Fargate covers the same migration for
// fargate/service mode, whose sidecar carries the xds address only in the envoy
// --config-yaml (the SSH/VPN container has no TrafficManagerService env).
func TestInjectedForManager_NamespaceMigration_Fargate(t *testing.T) {
	const (
		workloadNS = "default"
		nodeID     = "default.deployments.reviews"
	)
	spec := &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{Name: "reviews"}}}}

	AddEnvoyAndSSHContainer(spec, workloadNS, nodeID, false, "default", "img:test")
	if !injectedForManager(spec, "default") {
		t.Fatal("expected injectedForManager(default)=true right after fargate injection against default")
	}
	if injectedForManager(spec, "kubevpn") {
		t.Fatal("expected injectedForManager(kubevpn)=false: fargate envoy still targets default")
	}

	AddEnvoyAndSSHContainer(spec, workloadNS, nodeID, false, "kubevpn", "img:test")
	if !injectedForManager(spec, "kubevpn") {
		t.Fatal("expected injectedForManager(kubevpn)=true after fargate re-injection against kubevpn")
	}
	if n := countContainer(spec, config.ContainerSidecarEnvoy); n != 1 {
		t.Fatalf("expected exactly 1 envoy sidecar after fargate re-injection, got %d", n)
	}
}

// TestInjectedForManager_CRLF reproduces the Windows-only failure: the envoy
// config is embedded via go:embed, so a Windows checkout (git autocrlf) renders
// its lines with CRLF endings. injectedForManager must still recognize the baked
// traffic-manager address regardless of line ending. Simulate it on any platform
// by rewriting the rendered envoy arg's LF into CRLF.
func TestInjectedForManager_CRLF(t *testing.T) {
	secret := &v1.Secret{Data: map[string][]byte{}}
	spec := &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{Name: "authors"}}}}
	AddVPNAndEnvoyContainer(spec, "default", "default.deployments.authors", false, "default", secret, "img:test")

	for i := range spec.Spec.Containers {
		c := &spec.Spec.Containers[i]
		if c.Name != config.ContainerSidecarEnvoy {
			continue
		}
		for j := range c.Args {
			c.Args[j] = strings.ReplaceAll(c.Args[j], "\n", "\r\n")
		}
	}

	if !injectedForManager(spec, "default") {
		t.Fatal("expected injectedForManager(default)=true even when the envoy config uses CRLF line endings")
	}
	if injectedForManager(spec, "kubevpn") {
		t.Fatal("expected injectedForManager(kubevpn)=false: envoy still targets kubevpn-traffic-manager.default")
	}
}

func countContainer(spec *v1.PodTemplateSpec, name string) int {
	n := 0
	for _, c := range spec.Spec.Containers {
		if c.Name == name {
			n++
		}
	}
	return n
}
