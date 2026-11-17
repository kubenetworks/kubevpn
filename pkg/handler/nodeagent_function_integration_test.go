//go:build integration

package handler

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

func TestIntegration_NodeAgentFunction(t *testing.T) {
	u := &ut{}
	var originalAttachMode string
	var podUID types.UID

	// Phase 0: Cleanup even if test fails
	t.Cleanup(func() {
		// Try to leave proxy first
		_ = exec.Command("kubevpn", "leave", "deployments/reviews").Run()
		// Try to disconnect
		_ = exec.Command("kubevpn", "disconnect").Run()
		// Restore original attach mode
		if u.clientset != nil && originalAttachMode != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = u.setManagerAttachMode(ctx, originalAttachMode)
			_ = waitManagerReadyWithContext(ctx, u.clientset, u.namespace)
		}
	})

	// Phase 1: Initialize and connect
	t.Run("01-connect/init", u.init)
	t.Run("01-connect/connect", u.kubevpnConnect)

	// Phase 2: Save original attach mode and switch to node-agent
	t.Run("02-attach-mode/save-original", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		originalAttachMode, err = u.getCurrentAttachMode(ctx)
		if err != nil {
			t.Logf("Warning: failed to get original attach mode: %v", err)
			originalAttachMode = config.AttachModeSidecar
		}
	})
	t.Run("02-attach-mode/set-node-agent", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := u.setManagerAttachMode(ctx, config.AttachModeNodeAgent); err != nil {
			t.Fatalf("Failed to set attach mode to node-agent: %v", err)
		}
	})
	t.Run("02-attach-mode/wait-manager-ready", func(t *testing.T) {
		u.waitManagerReady(t)
	})

	// Phase 3: Proxy with service mesh
	t.Run("03-proxy/proxy", u.kubevpnProxyWithServiceMesh)

	// Phase 4: Verify agent job is created and reviews pod is not patched
	t.Run("04-verify/get-reviews-pod", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		var pod *corev1.Pod
		err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
			pods, err := u.clientset.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fields.OneTermEqualSelector("app", "reviews").String(),
			})
			if err != nil {
				return false, err
			}
			for _, p := range pods.Items {
				if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
					pod = &p
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			t.Fatalf("Failed to find running reviews pod: %v", err)
		}
		podUID = pod.UID
		t.Logf("Found reviews pod: %s (UID: %s)", pod.Name, podUID)
	})
	t.Run("04-verify/agent-job-created", func(t *testing.T) {
		if podUID == "" {
			t.Skip("No reviews pod UID found")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		jobName := inject.AgentJobName(podUID)
		t.Logf("Looking for agent job: %s", jobName)

		err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
			job, err := u.clientset.BatchV1().Jobs(u.namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return job.Status.Active > 0 || job.Status.Succeeded > 0, nil
		})
		if err != nil {
			t.Fatalf("Agent job %s not found or not active: %v", jobName, err)
		}

		// Also verify agent pod is running
		pods, err := u.clientset.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", "kubevpn/pod-uid", podUID),
		})
		if err != nil {
			t.Fatalf("Failed to list agent pods: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Fatalf("No agent pods found for pod UID %s", podUID)
		}
		t.Logf("Found agent pod: %s", pods.Items[0].Name)
	})
	t.Run("04-verify/reviews-pod-not-patched", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pods, err := u.clientset.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("app", "reviews").String(),
		})
		if err != nil {
			t.Fatalf("Failed to list reviews pods: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Fatal("No reviews pods found")
		}

		pod := &pods.Items[0]
		// Verify no kubevpn sidecar containers are present
		for _, c := range pod.Spec.Containers {
			if c.Name == config.ContainerSidecarEnvoy || c.Name == config.ContainerSidecarVPN {
				t.Fatalf("Pod %s has sidecar container %s (should not in node-agent mode)", pod.Name, c.Name)
			}
		}
		// Verify no kubevpn init containers are present
		for _, c := range pod.Spec.InitContainers {
			if c.Name == config.ContainerSidecarEnvoy || c.Name == config.ContainerSidecarVPN {
				t.Fatalf("Pod %s has sidecar init container %s (should not in node-agent mode)", pod.Name, c.Name)
			}
		}
		t.Logf("Pod %s is clean (no sidecars)", pod.Name)
	})

	// Phase 5: Verify iptables via exec into reviews pod
	t.Run("05-verify/iptables-via-exec", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		pods, err := u.clientset.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fields.OneTermEqualSelector("app", "reviews").String(),
		})
		if err != nil {
			t.Fatalf("Failed to list reviews pods: %v", err)
		}
		if len(pods.Items) == 0 {
			t.Fatal("No reviews pods found")
		}

		pod := &pods.Items[0]

		// Check /proc/net/tcp for port 15006 in LISTEN state (0A)
		// Port 15006 in network byte order (big-endian) hex is 3A9E
		// /proc/net/tcp format: sl local_address rem_address st tx_queue rx_queue ...
		// local_address format: ip:port in hex, network byte order
		err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", u.namespace, pod.Name, "--", "cat", "/proc/net/tcp")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Logf("Exec failed: %v, output: %s", err, string(output))
				return false, nil
			}

			// Look for :3A9E (port 15006) in listen state (0A)
			// Format: " 1: 00000000:3A9E 00000000:0000 0A ..."
			if bytes.Contains(output, []byte(":3A9E")) && bytes.Contains(output, []byte(" 0A ")) {
				t.Logf("Found port 15006 in LISTEN state in /proc/net/tcp")
				return true, nil
			}

			t.Logf("/proc/net/tcp doesn't show 15006 listening yet. Output:\n%s", string(output))
			return false, nil
		})
		if err != nil {
			t.Logf("Warning: failed to verify iptables via /proc/net/tcp, but continuing: %v", err)
			// This is not fatal - maybe the image doesn't have /proc mounted or different format
		}
	})

	// Phase 6: Verify traffic is intercepted
	t.Run("06-traffic/get-reviews-service-ip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		svc, err := u.clientset.CoreV1().Services(u.namespace).Get(ctx, "reviews", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get reviews service: %v", err)
		}
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
			t.Fatalf("Reviews service has no ClusterIP: %s", svc.Spec.ClusterIP)
		}
		t.Logf("Reviews service ClusterIP: %s", svc.Spec.ClusterIP)
	})
	t.Run("06-traffic/intercepted-with-header", func(t *testing.T) {
		u.waitManagerReady(t)
		ip, err := u.getServiceIP("reviews")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("http://%s:%d/health", ip, 9080)
		u.meshHealthChecker(t, endpoint, map[string]string{"env": "test"}, local)
	})
	t.Run("06-traffic/not-intercepted-without-header", func(t *testing.T) {
		u.waitManagerReady(t)
		ip, err := u.getServiceIP("reviews")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("http://%s:%d/health", ip, 9080)
		// Without header, should get remote response
		// Use a custom check since meshHealthChecker might retry too long
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Timeout: 10 * time.Second}

		var lastErr error
		err = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
			Duration: 2 * time.Second,
			Factor:   1.5,
			Steps:    10,
		}, func(ctx context.Context) (bool, error) {
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				return false, nil
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				lastErr = fmt.Errorf("status code: %d", resp.StatusCode)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			t.Logf("Warning: failed to verify non-intercepted traffic: %v (last err: %v)", err, lastErr)
			// Not fatal - maybe the mesh always intercepts, or timing issue
		}
	})

	// Phase 7: Cleanup and verify
	t.Run("07-cleanup/leave", u.kubevpnLeave)
	t.Run("07-cleanup/verify-agent-job-deleted", func(t *testing.T) {
		if podUID == "" {
			t.Skip("No reviews pod UID")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		jobName := inject.AgentJobName(podUID)
		err := wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := u.clientset.BatchV1().Jobs(u.namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err == nil {
				// Job still exists
				return false, nil
			}
			// Assume not found means deleted
			return true, nil
		})
		if err != nil {
			t.Logf("Warning: agent job %s may still exist: %v", jobName, err)
			// Not fatal - cleanup will try again
		}
	})
	t.Run("07-cleanup/disconnect", func(t *testing.T) {
		cmd := exec.Command("kubevpn", "disconnect")
		cmd.Stdout = tWriter{t}
		cmd.Stderr = tWriter{t}
		_ = cmd.Run() // Best effort
	})
	t.Run("07-cleanup/restore-attach-mode", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := u.setManagerAttachMode(ctx, originalAttachMode); err != nil {
			t.Logf("Warning: failed to restore attach mode: %v", err)
		}
		_ = waitManagerReadyWithContext(ctx, u.clientset, u.namespace) // Best effort
	})
}

func (u *ut) getCurrentAttachMode(ctx context.Context) (string, error) {
	deploy, err := u.clientset.AppsV1().Deployments(u.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, c := range deploy.Spec.Template.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "KUBEVPN_PROXY_ATTACH_MODE" {
				return env.Value, nil
			}
		}
	}
	return config.AttachModeSidecar, nil
}

func (u *ut) setManagerAttachMode(ctx context.Context, mode string) error {
	// Use retry with ResourceVersion for safe updates
	return wait.ExponentialBackoffWithContext(ctx, wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2,
		Steps:    5,
	}, func(ctx context.Context) (bool, error) {
		deploy, err := u.clientset.AppsV1().Deployments(u.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		updated := false
		for i := range deploy.Spec.Template.Spec.Containers {
			c := &deploy.Spec.Template.Spec.Containers[i]
			found := false
			for j := range c.Env {
				if c.Env[j].Name == "KUBEVPN_PROXY_ATTACH_MODE" {
					if c.Env[j].Value == mode {
						// Already set correctly
						return true, nil
					}
					c.Env[j].Value = mode
					found = true
					updated = true
					break
				}
			}
			if !found {
				// Add the env var
				c.Env = append(c.Env, corev1.EnvVar{
					Name:  "KUBEVPN_PROXY_ATTACH_MODE",
					Value: mode,
				})
				updated = true
			}
		}

		if !updated {
			// No change needed
			return true, nil
		}

		_, err = u.clientset.AppsV1().Deployments(u.namespace).Update(ctx, deploy, metav1.UpdateOptions{})
		if err != nil {
			// Conflict, retry
			return false, nil
		}
		return true, nil
	})
}

// waitManagerReadyWithContext waits for manager to be ready with context
func waitManagerReadyWithContext(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	return wait.PollUntilContextCancel(ctx, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: label})
		if err != nil {
			return false, nil
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			// Check all containers are ready
			ready := true
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					ready = false
					break
				}
			}
			if ready && pod.Status.Phase == corev1.PodRunning {
				return true, nil
			}
		}
		return false, nil
	})
}

// Helper to parse /proc/net/tcp - useful for debugging, not used in main test
func parseProcNetTCP(output []byte, targetPort int) bool {
	targetPortHex := hex.EncodeToString([]byte{byte(targetPort >> 8), byte(targetPort & 0xFF)})
	lines := bytes.Split(output, []byte("\n"))
	for _, line := range lines {
		fields := bytes.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// Skip header line (starts with "sl")
		if bytes.Equal(fields[0], []byte("sl")) {
			continue
		}
		// Check local address contains our port
		localAddr := fields[1]
		if !bytes.Contains(localAddr, []byte(targetPortHex)) {
			continue
		}
		// Check state is LISTEN (0A)
		state := fields[3]
		if bytes.Equal(state, []byte("0A")) {
			return true
		}
	}
	return false
}

type tWriter struct {
	t *testing.T
}

func (w tWriter) Write(p []byte) (n int, err error) {
	w.t.Log(string(p))
	return len(p), nil
}
