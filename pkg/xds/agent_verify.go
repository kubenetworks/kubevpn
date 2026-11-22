package xds

import (
	"bytes"
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// verifyInterval throttles the exec-based DNAT probe: at most one probe per
// target pod per interval, so the reconcile loop stays cheap.
const verifyInterval = time.Minute

// dnatProbeCommand reads the target pod netns' NAT table from inside the agent
// container. The vpn startup script records its own PID (a target-netns PID,
// since the script runs after kubevpn agent setns) in /tmp/kubevpn-agent-target-pid;
// we read that PID and nsenter its netns to check the NAT rules. Emits exactly
// one token: a count (0 = missing, >0 = present) on success, or "probe-unknown"
// on any failure (e.g., file missing/empty, nsenter/iptables fails).
const dnatProbeCommand = `pid=$(cat /tmp/kubevpn-agent-target-pid 2>/dev/null)
if [ -n "$pid" ] && out=$(nsenter -t "$pid" -n iptables -t nat -S 2>/dev/null); then
  echo "$out" | grep -c kubevpn-nodeagent || true
else
  echo probe-unknown
fi`

// parseDNATCount classifies probe output: (present, known). known=false means
// the probe could not tell — the caller must NOT churn the Job on unknown.
func parseDNATCount(output string) (present bool, known bool) {
	// Handle legacy two-line output ("0\nprobe-unknown") from the buggy grep -c || echo pattern
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var line string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			line = trimmed
			break
		}
	}
	switch {
	case line == "probe-unknown" || line == "":
		return false, false
	case line == "0":
		return false, true
	default:
		// If it's a positive number, present=true; otherwise (non-numeric) known=false
		if strings.TrimSpace(line) != "" && isNumeric(line) {
			return true, true
		}
		return false, false
	}
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// probeDNAT execs the probe command in the agent Job's vpn container. The
// rest config is derived lazily from the factory and cached.
func (r *AgentReconciler) probeDNAT(ctx context.Context, agentPodName string) (string, error) {
	if r.restConfig == nil {
		restConfig, err := r.factory.ToRESTConfig()
		if err != nil {
			return "", err
		}
		r.restConfig = restConfig
	}
	req := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(r.managerNamespace).Name(agentPodName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: "vpn",
		Command:   []string{"/bin/sh", "-c", dnatProbeCommand},
		Stdout:    true,
		Stderr:    true,
	}, runtime.NewParameterCodec(scheme.Scheme))
	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	if err = exec.Stream(remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		// Stderr is only for debugging; don't pollute the probe result
		plog.G(ctx).Debugf("[AgentReconciler] DNAT probe stderr for %s: %q", agentPodName, stderr.String())
		return "", err
	}
	plog.G(ctx).Debugf("[AgentReconciler] DNAT probe stderr for %s: %q", agentPodName, stderr.String())
	return stdout.String(), nil
}

// verifyDataPlane returns true when the target pod's netns still carries the
// kubevpn-nodeagent DNAT rules. Inconclusive results count as healthy: the
// reconciler must never churn a Job it could not actually judge.
func (r *AgentReconciler) verifyDataPlane(ctx context.Context, podUID types.UID, jobName string) bool {
	// If no factory is available (test environment), skip DNAT verification
	if r.factory == nil && r.probe == nil {
		return true
	}

	if r.lastVerify == nil {
		r.lastVerify = make(map[types.UID]time.Time)
	}
	if time.Since(r.lastVerify[podUID]) < verifyInterval {
		return true // recently verified
	}
	agentPod, found := r.findAgentPod(ctx, jobName)
	if !found {
		return true // health check handles missing pods
	}
	var (
		out string
		err error
	)
	if r.probe != nil {
		out, err = r.probe(ctx, agentPod.Name)
	} else {
		out, err = r.probeDNAT(ctx, agentPod.Name)
	}
	if err != nil {
		plog.G(ctx).Debugf("[AgentReconciler] DNAT probe for %s failed (kept): %v", jobName, err)
		return true
	}
	r.lastVerify[podUID] = time.Now()
	present, known := parseDNATCount(out)
	if !known {
		plog.G(ctx).Debugf("[AgentReconciler] DNAT probe for %s inconclusive (kept): %q", jobName, out)
		return true
	}
	if !present {
		plog.G(ctx).Infof("[AgentReconciler] DNAT rule missing for %s (probe=%q)", jobName, out)
	}
	return present
}
