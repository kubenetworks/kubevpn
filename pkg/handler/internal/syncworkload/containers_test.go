package syncworkload

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenSyncthingContainer(t *testing.T) {
	remoteDir := "/app/src"
	syncVolumeName := config.VolumeSyncthing
	image := "ghcr.io/kubenetworks/kubevpn:test"

	c := genSyncthingContainer(remoteDir, syncVolumeName, image)
	if c.Name != config.ContainerSidecarSyncthing || c.Image != image {
		t.Fatalf("unexpected syncthing identity: %s %s", c.Name, c.Image)
	}
	wantCmd := []string{"kubevpn", "syncthing", "--dir", remoteDir}
	if len(c.Command) != len(wantCmd) {
		t.Fatalf("command = %v, want %v", c.Command, wantCmd)
	}
	for i := range wantCmd {
		if c.Command[i] != wantCmd[i] {
			t.Fatalf("command[%d] = %q, want %q", i, c.Command[i], wantCmd[i])
		}
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != syncVolumeName || c.VolumeMounts[0].MountPath != remoteDir || c.VolumeMounts[0].ReadOnly {
		t.Fatalf("unexpected syncthing mount: %+v", c.VolumeMounts)
	}
	if _, ok := c.Resources.Requests[v1.ResourceCPU]; !ok {
		t.Fatal("missing CPU request")
	}
	if _, ok := c.Resources.Limits[v1.ResourceMemory]; !ok {
		t.Fatal("missing memory limit")
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 || c.SecurityContext.RunAsGroup == nil || *c.SecurityContext.RunAsGroup != 0 {
		t.Fatal("syncthing helper must run as root")
	}
}

func TestWaitPodReady_AlreadyRunning(t *testing.T) {
	readyPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status: v1.PodStatus{Phase: v1.PodRunning, Conditions: []v1.PodCondition{{
			Type: v1.PodReady, Status: v1.ConditionTrue,
		}}},
	}
	clientset := fake.NewSimpleClientset(readyPod)
	if err := clusterworkload.WaitPodReady(context.Background(), clientset.CoreV1(), "default", "app=web", ""); err != nil {
		t.Fatalf("WaitPodReady: %v", err)
	}
}
