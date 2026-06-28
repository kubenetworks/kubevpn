package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// findFlagValue returns the value following flag in argv, or "" if the flag is absent.
func findFlagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// vpnSidecarCommand runs prepareSyncPodSpec for the given SyncOptions and returns the
// command of the injected VPN sidecar (the nested `kubevpn proxy ...`) from the cloned
// workload's pod template. It exercises the real sync injection wiring end-to-end:
// container filtering, target-container resolution, genVPNContainer, and the
// unstructured round-trip — not a single function in isolation.
func vpnSidecarCommand(t *testing.T, d *SyncOptions) []string {
	t.Helper()
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "app", Image: "nginx:latest"}},
		},
	}
	u := &unstructured.Unstructured{Object: map[string]any{}}
	path := []string{"spec", "template"}

	err := d.prepareSyncPodSpec(context.Background(), syncPodSpec{
		spec:       spec,
		u:          u,
		workload:   "deployments.apps/authors",
		kubeconfig: []byte("{}"),
		image:      "ghcr.io/kubenetworks/kubevpn:test",
		args:       nil,
		path:       path,
		labels:     map[string]string{"origin-workload": "authors"},
	})
	if err != nil {
		t.Fatalf("prepareSyncPodSpec: %v", err)
	}

	for i := range spec.Spec.Containers {
		if spec.Spec.Containers[i].Name == config.ContainerSidecarVPN {
			return spec.Spec.Containers[i].Command
		}
	}
	t.Fatalf("vpn sidecar %q not found in cloned pod spec, containers=%v", config.ContainerSidecarVPN, spec.Spec.Containers)
	return nil
}

// TestSync_PinsManagerNamespace_WhenManagerDiffersFromWorkload verifies that when the
// traffic manager runs in a different namespace than the workload, the cloned sync pod's
// nested `kubevpn proxy` is pinned with --manager-namespace so the envoy sidecar it later
// injects points TrafficManagerService at the manager namespace (not the workload one).
func TestSync_PinsManagerNamespace_WhenManagerDiffersFromWorkload(t *testing.T) {
	d := &SyncOptions{
		WorkloadNamespace:   "app",
		ManagerNamespace:    "kubevpn-system",
		TargetWorkloadNames: map[string]string{},
	}
	cmd := vpnSidecarCommand(t, d)

	if ns, ok := findFlagValue(cmd, "--namespace"); !ok || ns != "app" {
		t.Fatalf("--namespace = %q (found=%v), want %q; cmd=%v", ns, ok, "app", cmd)
	}
	mgr, ok := findFlagValue(cmd, "--manager-namespace")
	if !ok {
		t.Fatalf("--manager-namespace not threaded into nested proxy command: %v", cmd)
	}
	if mgr != "kubevpn-system" {
		t.Fatalf("--manager-namespace = %q, want %q; cmd=%v", mgr, "kubevpn-system", cmd)
	}
}

// TestSync_OmitsManagerNamespace_WhenUnset verifies that without a resolved manager
// namespace, no --manager-namespace flag is added, preserving the legacy auto-detect
// behavior of the nested proxy.
func TestSync_OmitsManagerNamespace_WhenUnset(t *testing.T) {
	d := &SyncOptions{
		WorkloadNamespace:   "app",
		ManagerNamespace:    "",
		TargetWorkloadNames: map[string]string{},
	}
	cmd := vpnSidecarCommand(t, d)

	if _, ok := findFlagValue(cmd, "--manager-namespace"); ok {
		t.Fatalf("--manager-namespace should be absent when ManagerNamespace is empty; cmd=%v", cmd)
	}
	if cmd[len(cmd)-1] != "--foreground" {
		t.Fatalf("command should end with --foreground when no extra args; cmd=%v", cmd)
	}
}
