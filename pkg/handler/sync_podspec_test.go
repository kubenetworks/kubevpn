//go:build !integration

package handler

import (
	"context"
	"encoding/json"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// buildTestUnstructured builds a minimal Deployment-like unstructured object whose
// pod template is embedded at spec.template.spec.containers — the path returned by
// GetPodTemplateSpecPath for a Deployment.
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
		"spec": map[string]any{
			"template": templateMap,
		},
	}}
	// The pod template spec is stored at spec.template for a Deployment.
	path := []string{"spec", "template"}
	return u, path
}

// defaultSpec returns a minimal PodTemplateSpec with one app container.
func defaultSpec(containerName string) *v1.PodTemplateSpec {
	return &v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  containerName,
					Image: "app:v1",
					LivenessProbe: &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							Exec: &v1.ExecAction{Command: []string{"healthcheck"}},
						},
					},
					ReadinessProbe: &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							Exec: &v1.ExecAction{Command: []string{"ready"}},
						},
					},
				},
			},
		},
	}
}

func TestPrepareSyncPodSpec(t *testing.T) {
	image := "ghcr.io/kubenetworks/kubevpn:test"
	kubeconfig := []byte(`{"apiVersion":"v1"}`)

	type testcase struct {
		name              string
		opts              SyncOptions   // SyncOptions fields that drive the behaviour
		containerName     string        // name of the app container in the initial spec
		extraContainers   []v1.Container // pre-existing sidecar containers to be stripped
		wantContainerCount int           // total containers expected after mutation
		wantSidecarVPN    bool
		wantSidecarSync   bool
		wantImageOverride string         // if non-empty, app container image must equal this
		wantLabels        map[string]string
		wantAnnotationKey string         // annotation key that must be present
		wantVolumes       int            // minimum number of volumes expected
		wantSecCtxNonNil  bool           // spec.SecurityContext must be non-nil
		wantProbesNil     bool           // liveness/readiness probes cleared (LocalDir set)
		wantTailCommand   bool           // app container command changed to tail -f /dev/null
	}

	cases := []testcase{
		{
			name: "basic: adds VPN and syncthing sidecars, no local dir",
			opts: SyncOptions{
				TargetContainer:   "app",
				WorkloadNamespace: "default",
				ManagerNamespace:  "",
				LocalDir:          "",
				RemoteDir:         "/data",
			},
			containerName:      "app",
			wantContainerCount: 3, // app + vpn + syncthing
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{"env": "prod"},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2, // kubeconfig downwardAPI + syncthing emptyDir
			wantSecCtxNonNil:   true,
			wantProbesNil:      false,  // no LocalDir → probes kept
			wantTailCommand:    false,
		},
		{
			name: "with LocalDir: probes cleared and command overridden",
			opts: SyncOptions{
				TargetContainer:   "app",
				WorkloadNamespace: "ns1",
				ManagerNamespace:  "manager-ns",
				LocalDir:          "/local/src",
				RemoteDir:         "/remote/src",
			},
			containerName:      "app",
			wantContainerCount: 3,
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{"tier": "backend"},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2,
			wantSecCtxNonNil:   true,
			wantProbesNil:      true,  // LocalDir set → probes cleared
			wantTailCommand:    true,
		},
		{
			name: "image override: target image replaced on app container",
			opts: SyncOptions{
				TargetContainer:   "app",
				TargetImage:       "app:v2-debug",
				WorkloadNamespace: "default",
				LocalDir:          "",
				RemoteDir:         "/data",
			},
			containerName:      "app",
			wantContainerCount: 3,
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2,
			wantSecCtxNonNil:   true,
			wantImageOverride:  "app:v2-debug",
			wantProbesNil:      false,
			wantTailCommand:    false,
		},
		{
			name: "pre-existing VPN and envoy sidecars are stripped",
			opts: SyncOptions{
				TargetContainer:   "app",
				WorkloadNamespace: "default",
				LocalDir:          "",
				RemoteDir:         "/data",
			},
			containerName: "app",
			extraContainers: []v1.Container{
				{Name: config.ContainerSidecarVPN, Image: "old-vpn:v1"},
				{Name: config.ContainerSidecarEnvoy, Image: "old-envoy:v1"},
			},
			// old VPN + old envoy stripped; new VPN + syncthing added → still 3
			wantContainerCount: 3,
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2,
			wantSecCtxNonNil:   true,
			wantProbesNil:      false,
			wantTailCommand:    false,
		},
		{
			name: "nil security context is initialized",
			opts: SyncOptions{
				TargetContainer:   "app",
				WorkloadNamespace: "default",
				LocalDir:          "",
				RemoteDir:         "/data",
			},
			containerName:      "app",
			wantContainerCount: 3,
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2,
			wantSecCtxNonNil:   true,
			wantProbesNil:      false,
			wantTailCommand:    false,
		},
		{
			name: "empty target container name defaults to first container",
			opts: SyncOptions{
				TargetContainer:   "", // empty → FindOrDefaultContainerByName picks first
				WorkloadNamespace: "default",
				LocalDir:          "",
				RemoteDir:         "/data",
			},
			containerName:      "first-app",
			wantContainerCount: 3,
			wantSidecarVPN:     true,
			wantSidecarSync:    true,
			wantLabels:         map[string]string{},
			wantAnnotationKey:  config.KUBECONFIG,
			wantVolumes:        2,
			wantSecCtxNonNil:   true,
			wantProbesNil:      false,
			wantTailCommand:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := defaultSpec(tc.containerName)
			// Append any extra sidecar containers the test wants pre-populated.
			spec.Spec.Containers = append(spec.Spec.Containers, tc.extraContainers...)

			u, path := buildTestUnstructured(t, spec)

			labels := tc.wantLabels
			if labels == nil {
				labels = map[string]string{}
			}

			s := syncPodSpec{
				spec:       spec,
				u:          u,
				workload:   "deployments.apps/test",
				kubeconfig: kubeconfig,
				image:      image,
				args:       nil,
				path:       path,
				labels:     labels,
			}

			ctx := context.Background()
			err := tc.opts.prepareSyncPodSpec(ctx, s)
			if err != nil {
				t.Fatalf("prepareSyncPodSpec returned error: %v", err)
			}

			// --- Verify spec.Annotations contains the kubeconfig annotation ---
			anno := spec.GetAnnotations()
			if _, ok := anno[tc.wantAnnotationKey]; !ok {
				t.Errorf("annotation %q missing from spec", tc.wantAnnotationKey)
			}
			if anno[config.KUBECONFIG] != string(kubeconfig) {
				t.Errorf("annotation[%s] = %q, want %q", config.KUBECONFIG, anno[config.KUBECONFIG], string(kubeconfig))
			}

			// --- Verify labels propagated ---
			gotLabels := spec.GetLabels()
			for k, v := range tc.wantLabels {
				if gotLabels[k] != v {
					t.Errorf("label[%s] = %q, want %q", k, gotLabels[k], v)
				}
			}

			// --- Verify volumes ---
			if len(spec.Spec.Volumes) < tc.wantVolumes {
				t.Errorf("volumes count = %d, want >= %d", len(spec.Spec.Volumes), tc.wantVolumes)
			}
			hasKubeconfigVol := false
			hasSyncVol := false
			for _, vol := range spec.Spec.Volumes {
				if vol.Name == config.KUBECONFIG {
					hasKubeconfigVol = true
					if vol.VolumeSource.DownwardAPI == nil {
						t.Error("kubeconfig volume must use DownwardAPI")
					}
				}
				if vol.Name == config.VolumeSyncthing {
					hasSyncVol = true
					if vol.VolumeSource.EmptyDir == nil {
						t.Error("syncthing volume must use EmptyDir")
					}
				}
			}
			if !hasKubeconfigVol {
				t.Error("kubeconfig DownwardAPI volume not found")
			}
			if !hasSyncVol {
				t.Error("syncthing EmptyDir volume not found")
			}

			// --- Verify container count and names ---
			if len(spec.Spec.Containers) != tc.wantContainerCount {
				t.Errorf("container count = %d, want %d; containers: %v",
					len(spec.Spec.Containers), tc.wantContainerCount,
					containerNames(spec.Spec.Containers))
			}
			if tc.wantSidecarVPN && !hasContainer(spec.Spec.Containers, config.ContainerSidecarVPN) {
				t.Errorf("expected VPN sidecar %q, containers: %v", config.ContainerSidecarVPN, containerNames(spec.Spec.Containers))
			}
			if tc.wantSidecarSync && !hasContainer(spec.Spec.Containers, config.ContainerSidecarSyncthing) {
				t.Errorf("expected syncthing sidecar %q, containers: %v", config.ContainerSidecarSyncthing, containerNames(spec.Spec.Containers))
			}
			// Old envoy sidecar must be removed
			if hasContainer(spec.Spec.Containers, config.ContainerSidecarEnvoy) {
				t.Errorf("old envoy sidecar %q should have been stripped", config.ContainerSidecarEnvoy)
			}

			// --- Verify app container mutations ---
			appContainer := findContainer(spec.Spec.Containers, tc.containerName)
			if appContainer == nil {
				t.Fatalf("app container %q not found after mutation", tc.containerName)
			}
			if tc.wantImageOverride != "" && appContainer.Image != tc.wantImageOverride {
				t.Errorf("app container image = %q, want %q", appContainer.Image, tc.wantImageOverride)
			}
			if tc.wantProbesNil {
				if appContainer.LivenessProbe != nil {
					t.Error("liveness probe should be nil when LocalDir is set")
				}
				if appContainer.ReadinessProbe != nil {
					t.Error("readiness probe should be nil when LocalDir is set")
				}
			}
			if tc.wantTailCommand {
				if len(appContainer.Command) < 3 ||
					appContainer.Command[0] != "tail" ||
					appContainer.Command[1] != "-f" ||
					appContainer.Command[2] != "/dev/null" {
					t.Errorf("app container command = %v, want [tail -f /dev/null]", appContainer.Command)
				}
				if len(appContainer.Args) != 0 {
					t.Errorf("app container args should be empty, got %v", appContainer.Args)
				}
				// syncthing volume mount must be present
				hasSyncMount := false
				for _, vm := range appContainer.VolumeMounts {
					if vm.Name == config.VolumeSyncthing {
						hasSyncMount = true
						if vm.MountPath != tc.opts.RemoteDir {
							t.Errorf("syncthing volume mount path = %q, want %q", vm.MountPath, tc.opts.RemoteDir)
						}
					}
				}
				if !hasSyncMount {
					t.Error("syncthing volume mount not found in app container")
				}
			}

			// --- Verify security context initialized ---
			if tc.wantSecCtxNonNil && spec.Spec.SecurityContext == nil {
				t.Error("pod SecurityContext should be non-nil")
			}

			// --- Verify the mutation was propagated back into the unstructured object ---
			nested, found, err := unstructured.NestedMap(u.Object, path...)
			if err != nil || !found {
				t.Fatalf("spec not found in unstructured object at path %v: err=%v found=%v", path, err, found)
			}
			if nested == nil {
				t.Error("nested spec in unstructured is nil")
			}
		})
	}
}

// TestPrepareSyncPodSpec_UnknownContainerError verifies that specifying a
// TargetContainer that does not exist returns an error from
// FindOrDefaultContainerByName.
func TestPrepareSyncPodSpec_UnknownContainerError(t *testing.T) {
	spec := defaultSpec("app")
	u, path := buildTestUnstructured(t, spec)

	opts := SyncOptions{
		TargetContainer:   "nonexistent-container",
		WorkloadNamespace: "default",
	}
	s := syncPodSpec{
		spec:       spec,
		u:          u,
		workload:   "deployments.apps/test",
		kubeconfig: []byte("{}"),
		image:      "img:v1",
		path:       path,
		labels:     map[string]string{},
	}

	err := opts.prepareSyncPodSpec(context.Background(), s)
	if err == nil {
		t.Fatal("expected error for unknown target container, got nil")
	}
}

// TestPrepareSyncPodSpec_ManagerNamespace verifies that the manager namespace is
// forwarded to the VPN sidecar command when set.
func TestPrepareSyncPodSpec_ManagerNamespace(t *testing.T) {
	spec := defaultSpec("app")
	u, path := buildTestUnstructured(t, spec)

	opts := SyncOptions{
		TargetContainer:   "app",
		WorkloadNamespace: "workload-ns",
		ManagerNamespace:  "manager-ns",
		LocalDir:          "",
		RemoteDir:         "/data",
	}
	s := syncPodSpec{
		spec:       spec,
		u:          u,
		workload:   "deployments.apps/reviews",
		kubeconfig: []byte("{}"),
		image:      "img:v1",
		path:       path,
		labels:     map[string]string{},
	}

	if err := opts.prepareSyncPodSpec(context.Background(), s); err != nil {
		t.Fatalf("prepareSyncPodSpec: %v", err)
	}

	vpn := findContainer(spec.Spec.Containers, config.ContainerSidecarVPN)
	if vpn == nil {
		t.Fatal("VPN sidecar not found")
	}
	foundManagerNS := false
	for i, arg := range vpn.Command {
		if arg == "--manager-namespace" && i+1 < len(vpn.Command) && vpn.Command[i+1] == "manager-ns" {
			foundManagerNS = true
			break
		}
	}
	if !foundManagerNS {
		t.Errorf("--manager-namespace manager-ns not found in VPN container command: %v", vpn.Command)
	}
}

// --- helpers ---

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
