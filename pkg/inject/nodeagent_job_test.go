package inject

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func testAgentJobInput(pod *v1.Pod) AgentJobInput {
	return AgentJobInput{
		ManagerNamespace: "kubevpn",
		Namespace:        "default",
		NodeID:           "deployments.apps.reviews",
		Image:            config.Image,
		OwnerID:          "owner-1",
		Pod:              pod,
	}
}

func readyAgentPod(jobName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abc",
			Namespace: "kubevpn",
			Labels:    map[string]string{"job-name": jobName},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "vpn"}, {Name: "envoy"}},
		},
		Status: v1.PodStatus{
			Phase:             v1.PodRunning,
			ContainerStatuses: []v1.ContainerStatus{{Name: "vpn", Ready: true}, {Name: "envoy", Ready: true}},
		},
	}
}

func pendingAgentPod(jobName string) *v1.Pod {
	p := readyAgentPod(jobName)
	p.Status.Phase = v1.PodPending
	p.Status.ContainerStatuses = nil
	return p
}

func shortenTimeouts(t *testing.T, ready, terminating time.Duration) {
	t.Helper()
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = ready, terminating
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})
}

func TestEnsureAgentJob_ReturnsWhenPodReady(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1"}}
	jobName := AgentJobName(pod.UID)
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, readyAgentPod(jobName))

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob: %v", err)
	}
}

func TestEnsureAgentJob_TimesOutWhenPodNeverReady(t *testing.T) {
	shortenTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-2"}}
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, pendingAgentPod(AgentJobName(pod.UID)))

	err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod))
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("expected not-ready timeout, got %v", err)
	}
}

func TestEnsureAgentJob_WaitsTerminatingThenRecreates(t *testing.T) {
	shortenTimeouts(t, time.Second, 2*time.Second)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-3"}}
	jobName := AgentJobName(pod.UID)
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job)

	// Simulate the job disappearing and its replacement pod becoming ready.
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Fake clientset doesn't respect DeletionTimestamp; just delete the job directly
		_ = clientset.BatchV1().Jobs("kubevpn").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		// Create the new job and ready pod
		_, _ = clientset.BatchV1().Jobs("kubevpn").Create(context.Background(), GenAgentJobSpec(testAgentJobInput(pod)), metav1.CreateOptions{})
		_, _ = clientset.CoreV1().Pods("kubevpn").Create(context.Background(), readyAgentPod(AgentJobName(pod.UID)), metav1.CreateOptions{})
	}()

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob should recreate after terminating job disappears: %v", err)
	}
}

func TestVPNScriptIsConvergent(t *testing.T) {
	// We can't directly access vpnScript (it's inside GenAgentJobSpec),
	// so we generate the job spec and extract it from the container command.
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "test-uid"}}
	job := GenAgentJobSpec(testAgentJobInput(pod))
	var vpnScript string
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == "vpn" && len(c.Command) >= 10 {
			vpnScript = c.Command[9] // Command is ["kubevpn", "agent", "--pod-uid", uid, "--proc-root", "/host/proc", "--", "/bin/bash", "-c", vpnScript]
			break
		}
	}
	if vpnScript == "" {
		t.Fatal("vpn script not found in job spec")
	}

	// Define the expected flush loops and -A lines using the shared config constants
	flushIPv4 := `while iptables -t nat -D PREROUTING ` + config.AgentDNATRuleSpec4 + ` 2>/dev/null; do :; done`
	flushIPv6 := `while ip6tables -t nat -D PREROUTING ` + config.AgentDNATRuleSpec6 + ` 2>/dev/null; do :; done`
	addIPv4 := `iptables -t nat -A PREROUTING ` + config.AgentDNATRuleSpec4
	addIPv6 := `ip6tables -t nat -A PREROUTING ` + config.AgentDNATRuleSpec6

	// Check that all expected strings are present
	if !strings.Contains(vpnScript, flushIPv4) {
		t.Error("vpn script missing IPv4 flush loop")
	}
	if !strings.Contains(vpnScript, flushIPv6) {
		t.Error("vpn script missing IPv6 flush loop")
	}
	if !strings.Contains(vpnScript, addIPv4) {
		t.Error("vpn script missing IPv4 -A line")
	}
	if !strings.Contains(vpnScript, addIPv6) {
		t.Error("vpn script missing IPv6 -A line")
	}

	// Check that flush loops come before -A lines
	flushIPv4Idx := strings.Index(vpnScript, flushIPv4)
	addIPv4Idx := strings.Index(vpnScript, addIPv4)
	if flushIPv4Idx >= addIPv4Idx {
		t.Errorf("IPv4 flush loop must come before -A line, but %d >= %d", flushIPv4Idx, addIPv4Idx)
	}

	flushIPv6Idx := strings.Index(vpnScript, flushIPv6)
	addIPv6Idx := strings.Index(vpnScript, addIPv6)
	if flushIPv6Idx >= addIPv6Idx {
		t.Errorf("IPv6 flush loop must come before -A line, but %d >= %d", flushIPv6Idx, addIPv6Idx)
	}

	// Check that both env vars are present
	if !strings.Contains(vpnScript, "export KUBEVPN_AGENT_CLEANUP_IPTABLES=1") {
		t.Error("vpn script missing KUBEVPN_AGENT_CLEANUP_IPTABLES export")
	}
	if !strings.Contains(vpnScript, "export KUBEVPN_AGENT_MAINTAIN_IPTABLES=1") {
		t.Error("vpn script missing KUBEVPN_AGENT_MAINTAIN_IPTABLES export")
	}
}

// TestVPNScriptCarriesAgentIdentity guards the -l listener URL: it must carry
// agent=${POD_UID} (so kubevpn server leases its TUN IP as the agent pod UID,
// IsAgent=true) and must be quoted, because the & separator is a shell control
// operator in an unquoted word.
func TestVPNScriptCarriesAgentIdentity(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "test-uid"}}
	job := GenAgentJobSpec(testAgentJobInput(pod))
	var vpnScript string
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == "vpn" && len(c.Command) >= 10 {
			vpnScript = c.Command[9]
			break
		}
	}
	if vpnScript == "" {
		t.Fatal("vpn script not found in job spec")
	}

	const wantListener = `kubevpn server --debug -l "tun:/tcp://${TrafficManagerService}:10801?route=${CIDR4}&agent=${POD_UID}"`
	if !strings.Contains(vpnScript, wantListener) {
		t.Errorf("vpn script missing agent-identity listener URL:\nwant: %s", wantListener)
	}
}
