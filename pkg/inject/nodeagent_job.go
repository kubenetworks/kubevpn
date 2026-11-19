package inject

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// AgentJobInput captures the parameters needed to generate an agent Job spec.
// Extracted from InjectOptions to decouple the reconciler from the full injector state.
type AgentJobInput struct {
	ManagerNamespace string
	// Namespace is the target workload's namespace (NOT the manager's) — part
	// of the envoy node identifier.
	Namespace string
	// NodeID identifies the workload in the xDS control plane.
	NodeID string
	Image  string
	OwnerID string
	Secret  *v1.Secret
	Pod     *v1.Pod
}

// agentJobInput captures the parameters needed to generate an agent Job spec.
// Extracted from InjectOptions to decouple the reconciler from the full injector state.
// Deprecated: Use AgentJobInput instead.
type agentJobInput = AgentJobInput

// AgentJobName is deterministic per target pod, making create-or-skip
// idempotent for reconcile.
func AgentJobName(uid types.UID) string {
	return fmt.Sprintf("kubevpn-nodeagent-%s", uid)
}

// agentJobName is deterministic per target pod, making create-or-skip
// idempotent for reconcile.
// Deprecated: Use AgentJobName instead.
func agentJobName(uid types.UID) string {
	return AgentJobName(uid)
}

// GenAgentJobSpec builds the privileged, node-pinned agent Job (docs/55 §4).
// Two containers mirror the mesh sidecar pair; both enter the pod netns via
// `kubevpn agent` before exec'ing their payload.
func GenAgentJobSpec(input AgentJobInput) *batchv1.Job {
	privileged := true
	tlsEnv := []v1.EnvVar{}
	if input.Secret != nil {
		tlsEnv = tlsEnvVars(input.Secret)
	}
	commonEnv := append([]v1.EnvVar{
		{Name: "CIDR4", Value: config.CIDR.String()},
		{Name: "CIDR6", Value: config.CIDR6.String()},
		{Name: "TrafficManagerService", Value: trafficManagerAddr(input.ManagerNamespace)},
		{Name: "POD_UID", Value: string(input.Pod.UID)},
		{Name: "ENVOY_NODE_ID", Value: util.GenEnvoyUID(input.Namespace, input.NodeID)},
	}, tlsEnv...)

	vpnScript := `echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 0 > /proc/sys/net/ipv4/tcp_timestamps
for b in iptables-nft iptables-legacy; do "$b" -t nat -L &>/dev/null && update-alternatives --set iptables /usr/sbin/$b && update-alternatives --set ip6tables /usr/sbin/${b/iptables/ip6tables} && break; done
iptables -P INPUT ACCEPT
ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT
ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006 -m comment --comment "kubevpn-nodeagent"
ip6tables -t nat -A PREROUTING -p tcp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006 -m comment --comment "kubevpn-nodeagent"
export KUBEVPN_AGENT_CLEANUP_IPTABLES=1
exec kubevpn server --debug -l tun:/tcp://${TrafficManagerService}:10801?route=${CIDR4}
`
	// The envoy payload renders the same embedded bootstrap template the mesh
	// envoy sidecar uses, with the same node identifier the sidecar passes
	// via --service-node/--service-cluster (ADS requires node id + cluster).
	envoyScript := `mkdir -p /etc/kubevpn
kubevpn agent-envoy-config --manager ${TrafficManagerService} > /etc/kubevpn/envoy.yaml
exec envoy -c /etc/kubevpn/envoy.yaml --log-level warning --service-node ${ENVOY_NODE_ID} --service-cluster ${ENVOY_NODE_ID}
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentJobName(input.Pod.UID),
			Namespace: input.ManagerNamespace,
			Labels: map[string]string{
				"kubevpn/owner-id": input.OwnerID,
				"kubevpn/pod-uid":  string(input.Pod.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"kubevpn/owner-id": input.OwnerID,
					"kubevpn/pod-uid":  string(input.Pod.UID),
				}},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": input.Pod.Spec.NodeName},
					HostPID:       true,
					Volumes: []v1.Volume{{
						Name: "host-proc",
						VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{
							Path: "/proc", Type: ptr.To(v1.HostPathDirectory),
						}},
					}},
					Containers: []v1.Container{
						{
							Name:    "vpn",
							Image:   input.Image,
							Command: []string{"kubevpn", "agent", "--pod-uid", string(input.Pod.UID), "--proc-root", "/host/proc", "--", "/bin/bash", "-c", vpnScript},
							Env:     commonEnv,
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []v1.VolumeMount{{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true}},
						},
						{
							Name:    "envoy",
							Image:   input.Image,
							Command: []string{"kubevpn", "agent", "--pod-uid", string(input.Pod.UID), "--proc-root", "/host/proc", "--", "/bin/bash", "-c", envoyScript},
							Env:     commonEnv,
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []v1.VolumeMount{{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true}},
						},
					},
				},
			},
		},
	}
	return job
}

// genAgentJobSpec builds the privileged, node-pinned agent Job (docs/55 §4).
// Two containers mirror the mesh sidecar pair; both enter the pod netns via
// `kubevpn agent` before exec'ing their payload.
// Deprecated: Use GenAgentJobSpec instead.
func genAgentJobSpec(input AgentJobInput) *batchv1.Job {
	return GenAgentJobSpec(input)
}
