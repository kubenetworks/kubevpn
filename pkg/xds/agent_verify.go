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
	_ "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	_ "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// verifyInterval throttles the exec-based DNAT probe: at most one probe per
// target pod per interval, so the reconcile loop stays cheap.
const verifyInterval = time.Minute

// dnatProbeCommand reads the target pod netns' NAT table from inside the agent
// container. The agent's main process (PID 1) has setns'd into the TARGET
// pod's netns, so `nsenter -t 1 -n` re-enters it — the container's own netns
// (the agent pod) does not carry the rules. `|| echo probe-unknown` keeps a
// missing nsenter (exit 127) from looking like a transport error.
const dnatProbeCommand = `nsenter -t 1 -n iptables -t nat -S 2>/dev/null | grep -c kubevpn-nodeagent || echo probe-unknown`

// parseDNATCount classifies probe output: (present, known). known=false means
// the probe could not tell — the caller must NOT churn the Job on unknown.
func parseDNATCount(output string) (present bool, known bool) {
	line := strings.TrimSpace(output)
	switch {
	case line == "probe-unknown" || line == "":
		return false, false
	case line == "0":
		return false, true
	default:
		return true, true
	}
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
	var out bytes.Buffer
	if err = exec.Stream(remotecommand.StreamOptions{Stdout: &out, Stderr: &out}); err != nil {
		return "", err
	}
	return out.String(), nil
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
