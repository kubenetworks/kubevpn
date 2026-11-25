//go:build integration

package handler

import (
	"context"
	"os/exec"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pkgconfig "github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestIntegration_NodeBoundaryFunction exercises ordinary proxy without any
// sidecar: attach, mesh, portmap, leave/re-proxy and restart convergence.
func TestIntegration_NodeBoundaryFunction(t *testing.T) {
	u := &sshUt{}
	t.Run("01-connect/init", u.init)
	t.Run("01-connect/connect", u.kubevpnConnect)

	t.Run("02-full-proxy/nonintrusive", func(t *testing.T) {
		podsBefore, err := u.clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{LabelSelector: "app=reviews"})
		if err != nil || len(podsBefore.Items) == 0 {
			t.Fatalf("list reviews before attach: %v", err)
		}
		uid := podsBefore.Items[0].UID
		generation, err := u.clientset.AppsV1().Deployments("default").Get(context.Background(), "reviews", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}

		u.kubevpnProxy(t)

		podsAfter, err := u.clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{LabelSelector: "app=reviews"})
		if err != nil || len(podsAfter.Items) == 0 {
			t.Fatalf("list reviews after attach: %v", err)
		}
		if podsAfter.Items[0].UID != uid {
			t.Fatal("attach restarted the target pod")
		}
		deployAfter, err := u.clientset.AppsV1().Deployments("default").Get(context.Background(), "reviews", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if deployAfter.Generation != generation.Generation {
			t.Fatal("attach changed the workload generation")
		}
		for _, c := range podsAfter.Items[0].Spec.Containers {
			if c.Name == pkgconfig.ContainerSidecarVPN || c.Name == pkgconfig.ContainerSidecarEnvoy {
				t.Fatalf("sidecar found in ordinary proxy pod: %s", c.Name)
			}
		}
	})

	t.Run("03-mesh/portmap/leave-reproxy", func(t *testing.T) {
		u.kubevpnProxyWithServiceMesh(t)
		u.serviceMeshReviewsServiceIP(t)
		u.kubevpnLeave(t)
		u.kubevpnProxyWithServiceMeshAndK8sServicePortMap(t)
		u.serviceMeshReviewsServiceIPPortMap(t)
	})

	t.Run("04-agent-restart", func(t *testing.T) {
		cmd := exec.Command("kubectl", "delete", "pod", "-n", "kubevpn", "-l", "app=kubevpn-nodeagent", "--wait=true")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete node agent: %v: %s", err, output)
		}
		u.serviceMeshReviewsServiceIP(t)
	})

	t.Run("05-target-pod-restart", func(t *testing.T) {
		cmd := exec.Command("kubectl", "delete", "pod", "-n", "default", "-l", "app=reviews", "--wait=true")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete target pod: %v: %s", err, output)
		}
		u.serviceMeshReviewsServiceIP(t)
	})

	t.Run("06-manager-restart", func(t *testing.T) {
		cmd := exec.Command("kubectl", "delete", "pod", "-n", "kubevpn", "-l", "app=kubevpn-traffic-manager", "--wait=true")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete traffic manager: %v: %s", err, output)
		}
		u.serviceMeshReviewsServiceIP(t)
	})

	t.Run("07-no-per-pod-jobs", func(t *testing.T) {
		jobs, err := u.clientset.BatchV1().Jobs(u.namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs.Items {
			if job.Labels["kubevpn/pod-uid"] != "" {
				t.Fatalf("unexpected per-pod node-agent job: %s/%s", job.Namespace, job.Name)
			}
		}
	})

	t.Run("08-quit", u.kubevpnQuit)
}
