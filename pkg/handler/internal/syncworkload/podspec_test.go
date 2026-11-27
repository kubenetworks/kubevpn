//go:build !integration

package syncworkload

import (
	"context"
	"encoding/json"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func buildTestUnstructured(t *testing.T, spec *v1.PodTemplateSpec) (*unstructured.Unstructured, []string) {
	t.Helper()
	templateBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal spec: %v", err)
	}
	var templateMap map[string]any
	if err = json.Unmarshal(templateBytes, &templateMap); err != nil {
		t.Fatalf("json.Unmarshal spec: %v", err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "test-deploy", "namespace": "default"},
		"spec":       map[string]any{"template": templateMap},
	}}
	return u, []string{"spec", "template"}
}

func defaultSpec(containerName string) *v1.PodTemplateSpec {
	return &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{
		Name: containerName, Image: "app:v1",
		LivenessProbe:  &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"healthcheck"}}}},
		ReadinessProbe: &v1.Probe{ProbeHandler: v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"ready"}}}},
	}}}}
}

func TestPrepareSyncPodSpec(t *testing.T) {
	const image = "ghcr.io/kubenetworks/kubevpn:test"
	tests := []struct {
		name              string
		opts              Config
		containerName     string
		wantImageOverride string
		wantLabels        map[string]string
		wantProbesNil     bool
		wantTailCommand   bool
	}{
		{
			name:          "basic adds only syncthing helper",
			opts:          Config{TargetContainer: "app", WorkloadNamespace: "default", RemoteDir: "/data"},
			containerName: "app", wantLabels: map[string]string{"env": "prod"},
		},
		{
			name:          "local dir clears probes and overrides command",
			opts:          Config{TargetContainer: "app", WorkloadNamespace: "ns1", LocalDir: "/local/src", RemoteDir: "/remote/src"},
			containerName: "app", wantLabels: map[string]string{"tier": "backend"}, wantProbesNil: true, wantTailCommand: true,
		},
		{
			name:          "image override",
			opts:          Config{TargetContainer: "app", TargetImage: "app:v2-debug", WorkloadNamespace: "default", RemoteDir: "/data"},
			containerName: "app", wantLabels: map[string]string{}, wantImageOverride: "app:v2-debug",
		},
		{
			name:          "nil security context is initialized",
			opts:          Config{TargetContainer: "app", WorkloadNamespace: "default", RemoteDir: "/data"},
			containerName: "app", wantLabels: map[string]string{},
		},
		{
			name:          "empty target container defaults to first",
			opts:          Config{WorkloadNamespace: "default", RemoteDir: "/data"},
			containerName: "first-app", wantLabels: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := defaultSpec(tt.containerName)
			u, path := buildTestUnstructured(t, spec)
			err := tt.opts.prepareSyncPodSpec(context.Background(), syncPodSpec{
				spec: spec, u: u, image: image, path: path, labels: tt.wantLabels,
			})
			if err != nil {
				t.Fatalf("prepareSyncPodSpec: %v", err)
			}
			if _, ok := spec.Annotations[config.KUBECONFIG]; ok {
				t.Fatal("sync clone must not carry a nested-proxy kubeconfig")
			}
			for k, v := range tt.wantLabels {
				if got := spec.Labels[k]; got != v {
					t.Errorf("label[%s] = %q, want %q", k, got, v)
				}
			}
			if len(spec.Spec.Volumes) != 1 || spec.Spec.Volumes[0].Name != config.VolumeSyncthing || spec.Spec.Volumes[0].EmptyDir == nil {
				t.Fatalf("expected only the syncthing EmptyDir volume: %+v", spec.Spec.Volumes)
			}
			if len(spec.Spec.Containers) != 2 {
				t.Fatalf("expected app + syncthing, got %v", containerNames(spec.Spec.Containers))
			}
			if hasContainer(spec.Spec.Containers, config.ContainerSidecarVPN) || hasContainer(spec.Spec.Containers, config.ContainerSidecarEnvoy) {
				t.Fatalf("traffic sidecar found in sync clone: %v", containerNames(spec.Spec.Containers))
			}
			if !hasContainer(spec.Spec.Containers, config.ContainerSidecarSyncthing) {
				t.Fatalf("syncthing helper missing: %v", containerNames(spec.Spec.Containers))
			}
			app := findContainer(spec.Spec.Containers, tt.containerName)
			if app == nil {
				t.Fatalf("app container %q not found", tt.containerName)
			}
			if tt.wantImageOverride != "" && app.Image != tt.wantImageOverride {
				t.Errorf("app image = %q, want %q", app.Image, tt.wantImageOverride)
			}
			if tt.wantProbesNil && (app.LivenessProbe != nil || app.ReadinessProbe != nil || app.StartupProbe != nil) {
				t.Error("probes must be cleared when LocalDir is set")
			}
			if tt.wantTailCommand {
				if len(app.Command) != 3 || app.Command[0] != "tail" || app.Command[1] != "-f" || app.Command[2] != "/dev/null" || len(app.Args) != 0 {
					t.Errorf("unexpected app command/args: %v %v", app.Command, app.Args)
				}
				foundMount := false
				for _, mount := range app.VolumeMounts {
					if mount.Name == config.VolumeSyncthing && mount.MountPath == tt.opts.RemoteDir {
						foundMount = true
					}
				}
				if !foundMount {
					t.Error("syncthing volume mount missing from app")
				}
			}
			if spec.Spec.SecurityContext == nil {
				t.Error("pod SecurityContext should be initialized")
			}
			nested, found, err := unstructured.NestedMap(u.Object, path...)
			if err != nil || !found || nested == nil {
				t.Fatalf("mutated template was not propagated: found=%v err=%v", found, err)
			}
		})
	}
}

func TestPrepareSyncPodSpec_UnknownContainerError(t *testing.T) {
	spec := defaultSpec("app")
	u, path := buildTestUnstructured(t, spec)
	opts := Config{TargetContainer: "nonexistent-container", WorkloadNamespace: "default"}
	err := opts.prepareSyncPodSpec(context.Background(), syncPodSpec{spec: spec, u: u, image: "img:v1", path: path, labels: map[string]string{}})
	if err == nil {
		t.Fatal("expected unknown target container error")
	}
}

func hasContainer(containers []v1.Container, name string) bool {
	return findContainer(containers, name) != nil
}

func findContainer(containers []v1.Container, name string) *v1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func containerNames(containers []v1.Container) []string {
	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}
	return names
}
