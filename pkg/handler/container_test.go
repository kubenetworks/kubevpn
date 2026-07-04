package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenVPNContainer(t *testing.T) {
	workload := "deployments.apps/reviews"
	namespace := "test-ns"
	image := "ghcr.io/kubenetworks/kubevpn:test"
	args := []string{"--headers", "env=test"}

	c := genVPNContainer(workload, namespace, image, args)

	if c.Name != config.ContainerSidecarVPN {
		t.Fatalf("container name = %q, want %q", c.Name, config.ContainerSidecarVPN)
	}
	if c.Image != image {
		t.Fatalf("image = %q, want %q", c.Image, image)
	}

	// Command must start with "kubevpn proxy <workload>"
	if len(c.Command) < 3 {
		t.Fatalf("command too short: %v", c.Command)
	}
	if c.Command[0] != "kubevpn" || c.Command[1] != "proxy" || c.Command[2] != workload {
		t.Fatalf("command prefix = %v, want [kubevpn proxy %s ...]", c.Command[:3], workload)
	}

	// Must contain --namespace flag with correct value
	foundNS := false
	for i, arg := range c.Command {
		if arg == "--namespace" && i+1 < len(c.Command) && c.Command[i+1] == namespace {
			foundNS = true
			break
		}
	}
	if !foundNS {
		t.Fatalf("--namespace %s not found in command: %v", namespace, c.Command)
	}

	// Must contain --image flag
	foundImage := false
	for i, arg := range c.Command {
		if arg == "--image" && i+1 < len(c.Command) && c.Command[i+1] == image {
			foundImage = true
			break
		}
	}
	if !foundImage {
		t.Fatalf("--image %s not found in command: %v", image, c.Command)
	}

	// Extra args (--headers env=test) must be appended
	foundHeaders := false
	for i, arg := range c.Command {
		if arg == "--headers" && i+1 < len(c.Command) && c.Command[i+1] == "env=test" {
			foundHeaders = true
			break
		}
	}
	if !foundHeaders {
		t.Fatalf("extra args not appended to command: %v", c.Command)
	}

	// Must have kubeconfig volume mount
	if len(c.VolumeMounts) == 0 {
		t.Fatal("no volume mounts")
	}
	if c.VolumeMounts[0].Name != config.KUBECONFIG {
		t.Fatalf("volume mount name = %q, want %q", c.VolumeMounts[0].Name, config.KUBECONFIG)
	}
	if c.VolumeMounts[0].MountPath != "/tmp/.kube" {
		t.Fatalf("volume mount path = %q, want /tmp/.kube", c.VolumeMounts[0].MountPath)
	}

	// Must have NET_ADMIN capability
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		t.Fatal("missing security context / capabilities")
	}
	hasNetAdmin := false
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if cap == "NET_ADMIN" {
			hasNetAdmin = true
		}
	}
	if !hasNetAdmin {
		t.Fatal("NET_ADMIN capability not found")
	}

	// Must have resource requests and limits
	if _, ok := c.Resources.Requests[v1.ResourceCPU]; !ok {
		t.Fatal("missing CPU request")
	}
	if _, ok := c.Resources.Limits[v1.ResourceMemory]; !ok {
		t.Fatal("missing memory limit")
	}

	// Must have lifecycle postStart hook
	if c.Lifecycle == nil || c.Lifecycle.PostStart == nil {
		t.Fatal("missing lifecycle postStart")
	}
}

func TestGenVPNContainer_NoExtraArgs(t *testing.T) {
	c := genVPNContainer("deploy/web", "default", "img:v1", nil)

	if c.Name != config.ContainerSidecarVPN {
		t.Fatalf("container name = %q, want %q", c.Name, config.ContainerSidecarVPN)
	}
	// Command should end with --foreground (no extra args appended)
	last := c.Command[len(c.Command)-1]
	if last != "--foreground" {
		t.Fatalf("last command arg = %q, want --foreground", last)
	}
}

func TestGenSyncthingContainer(t *testing.T) {
	remoteDir := "/app/src"
	syncVolumeName := config.VolumeSyncthing
	image := "ghcr.io/kubenetworks/kubevpn:test"

	c := genSyncthingContainer(remoteDir, syncVolumeName, image)

	if c.Name != config.ContainerSidecarSyncthing {
		t.Fatalf("container name = %q, want %q", c.Name, config.ContainerSidecarSyncthing)
	}
	if c.Image != image {
		t.Fatalf("image = %q, want %q", c.Image, image)
	}

	// Command must be: kubevpn syncthing --dir <remoteDir>
	wantCmd := []string{"kubevpn", "syncthing", "--dir", remoteDir}
	if len(c.Command) != len(wantCmd) {
		t.Fatalf("command = %v, want %v", c.Command, wantCmd)
	}
	for i := range wantCmd {
		if c.Command[i] != wantCmd[i] {
			t.Fatalf("command[%d] = %q, want %q", i, c.Command[i], wantCmd[i])
		}
	}

	// Must mount the sync volume at remoteDir
	if len(c.VolumeMounts) != 1 {
		t.Fatalf("expected 1 volume mount, got %d", len(c.VolumeMounts))
	}
	vm := c.VolumeMounts[0]
	if vm.Name != syncVolumeName {
		t.Fatalf("volume mount name = %q, want %q", vm.Name, syncVolumeName)
	}
	if vm.MountPath != remoteDir {
		t.Fatalf("volume mount path = %q, want %q", vm.MountPath, remoteDir)
	}
	if vm.ReadOnly {
		t.Fatal("volume mount should not be read-only")
	}

	// Must have resource requests and limits
	if _, ok := c.Resources.Requests[v1.ResourceCPU]; !ok {
		t.Fatal("missing CPU request")
	}
	if _, ok := c.Resources.Limits[v1.ResourceMemory]; !ok {
		t.Fatal("missing memory limit")
	}

	// SecurityContext must run as root
	if c.SecurityContext == nil {
		t.Fatal("missing security context")
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Fatalf("RunAsUser = %v, want 0", c.SecurityContext.RunAsUser)
	}
	if c.SecurityContext.RunAsGroup == nil || *c.SecurityContext.RunAsGroup != 0 {
		t.Fatalf("RunAsGroup = %v, want 0", c.SecurityContext.RunAsGroup)
	}
}

func TestWaitPodReady_AlreadyRunning(t *testing.T) {
	readyPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}

	clientset := fake.NewSimpleClientset(readyPod)
	podClient := clientset.CoreV1().Pods("default")

	ctx := context.Background()
	err := WaitPodReady(ctx, podClient, "app=web")
	if err != nil {
		t.Fatalf("WaitPodReady returned error for already-running pod: %v", err)
	}
}
