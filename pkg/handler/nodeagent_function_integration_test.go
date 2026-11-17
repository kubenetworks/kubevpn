//go:build integration

package handler

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

func TestIntegration_NodeAgentFunction(t *testing.T) {
	u := &ut{}
	var originalAttachMode string
	var podUID types.UID
	var agentPodName string

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
		agentPodName = pods.Items[0].Name
		t.Logf("Found agent pod: %s", agentPodName)
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
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		var lastOutput []byte
		err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
			cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", u.namespace, pod.Name, "--", "cat", "/proc/net/tcp")
			output, err := cmd.CombinedOutput()
			lastOutput = output
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
			if agentPodName != "" {
				dumpAgentPodDiagnostics(t, ctx, u.clientset, u.namespace, agentPodName)
			}
			t.Fatalf("Failed to verify iptables via /proc/net/tcp: %v. Last output:\n%s", err, string(lastOutput))
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
			if agentPodName != "" {
				dumpAgentPodDiagnostics(t, context.Background(), u.clientset, u.namespace, agentPodName)
			}
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("http://%s:%d/health", ip, 9080)
		u.meshHealthChecker(t, endpoint, map[string]string{"env": "test"}, local)
	})
	t.Run("06-traffic/not-intercepted-without-header", func(t *testing.T) {
		u.waitManagerReady(t)
		ip, err := u.getServiceIP("reviews")
		if err != nil {
			if agentPodName != "" {
				dumpAgentPodDiagnostics(t, context.Background(), u.clientset, u.namespace, agentPodName)
			}
			t.Fatal(err)
		}
		endpoint := fmt.Sprintf("http://%s:%d/health", ip, 9080)
		// Without header, should get remote response

		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}

		client := &http.Client{Timeout: 5 * time.Second}
		err = retry.OnError(
			healthCheckBackoff(),
			func(err error) bool { return err != nil },
			func() error {
				var resp *http.Response
				resp, err = client.Do(req)
				if err != nil {
					t.Logf("%s failed to do health check endpoint %s: %v", time.Now().Format(time.DateTime), endpoint, err)
					return err
				}
				if resp.StatusCode != 200 {
					if resp.Body != nil {
						defer resp.Body.Close()
						all, _ := io.ReadAll(resp.Body)
						return fmt.Errorf("status code is %s, content: %v", resp.Status, string(all))
					}
					return fmt.Errorf("status code is %s", resp.Status)
				}
				defer resp.Body.Close()
				content, err := io.ReadAll(resp.Body)
				if err != nil {
					return err
				}
				if string(content) != remote {
					return fmt.Errorf("response=%s, want: %s", string(content), remote)
				}
				return nil
			},
		)
		if err != nil {
			if agentPodName != "" {
				dumpAgentPodDiagnostics(t, context.Background(), u.clientset, u.namespace, agentPodName)
			}
			t.Fatalf("Failed to verify non-intercepted traffic (remote response not received): %v", err)
		}
	})

	// Phase 7: Cleanup and verify
	t.Run("07-cleanup/leave", u.kubevpnLeave)
	t.Run("07-cleanup/verify-agent-job-deleted", func(t *testing.T) {
		if podUID == "" {
			t.Skip("No reviews pod UID")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		jobName := inject.AgentJobName(podUID)
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := u.clientset.BatchV1().Jobs(u.namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err == nil {
				// Job still exists
				return false, nil
			}
			if k8serrors.IsNotFound(err) {
				// Job deleted
				return true, nil
			}
			// Other error, keep polling
			return false, nil
		})
		if err != nil {
			t.Fatalf("Agent job %s still exists after leave: %v", jobName, err)
		}
	})
	t.Run("07-cleanup/disconnect", func(t *testing.T) {
		cmd := exec.Command("kubevpn", "disconnect")
		cmd.StdOut = tWriter{t}
		cmd.StdErr = tWriter{t}
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

// dumpAgentPodDiagnostics logs diagnostic information about an agent pod on failure.
func dumpAgentPodDiagnostics(t *testing.T, ctx context.Context, clientset kubernetes.Interface, namespace, podName string) {
	t.Helper()
	t.Log("========== AGENT POD DIAGNOSTICS ==========")

	// 1. Get pod details
	pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Logf("Failed to get agent pod: %v", err)
	} else {
		t.Logf("Agent pod: %s (phase: %s, node: %s)", pod.Name, pod.Status.Phase, pod.Spec.NodeName)
		t.Logf("Container statuses:")
		for _, cs := range pod.Status.ContainerStatuses {
			var stateStr string
			if cs.State.Running != nil {
				stateStr = "Running"
			} else if cs.State.Terminated != nil {
				stateStr = fmt.Sprintf("Terminated (exit: %d, reason: %s)",
					cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
			} else if cs.State.Waiting != nil {
				stateStr = fmt.Sprintf("Waiting (reason: %s)", cs.State.Waiting.Reason)
			} else {
				stateStr = fmt.Sprintf("%v", cs.State)
			}
			t.Logf("  - %s: ready=%v, restartCount=%d, state=%s",
				cs.Name, cs.Ready, cs.RestartCount, stateStr)
			if cs.LastTerminationState.Terminated != nil {
				t.Logf("    Last termination: exit=%d, reason=%s",
					cs.LastTerminationState.Terminated.ExitCode,
					cs.LastTerminationState.Terminated.Reason)
			}
		}
	}

	// 2. Get logs from each container
	containerNames := []string{"vpn", "envoy"} // Standard agent container names
	for _, container := range containerNames {
		t.Logf("--- %s container logs (last 100 lines) ---", container)
		req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: container,
			TailLines: ptr.To(int64(100)),
		})
		logs, err := req.DoRaw(ctx)
		if err != nil {
			t.Logf("Failed to get %s logs: %v", container, err)
		} else {
			t.Logf("%s", string(logs))
		}
	}

	// 3. kubectl get pod -o wide output
	t.Log("--- kubectl get pod -o wide ---")
	cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", "-n", namespace, podName, "-o", "wide")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("kubectl get pod failed: %v", err)
	} else {
		t.Logf("%s", string(output))
	}

	t.Log("========== END AGENT POD DIAGNOSTICS ==========")
}
