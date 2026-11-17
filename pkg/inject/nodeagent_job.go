package inject

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// agentJobName is deterministic per target pod, making create-or-skip
// idempotent for reconcile.
func agentJobName(uid types.UID) string {
	return fmt.Sprintf("kubevpn-nodeagent-%s", uid)
}

// genAgentJobSpec builds the privileged, node-pinned agent Job (docs/55 §4).
// Two containers mirror the mesh sidecar pair; both enter the pod netns via
// `kubevpn agent` before exec'ing their payload.
func genAgentJobSpec(opts InjectOptions, pod *v1.Pod) *batchv1.Job {
	privileged := true
	tlsEnv := []v1.EnvVar{}
	if opts.Secret != nil {
		tlsEnv = []v1.EnvVar{
			{Name: "tls_crt", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_crt"}}},
			{Name: "tls_key", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_key"}}},
			{Name: "tls_server_name", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_server_name"}}},
		}
	}
	commonEnv := append([]v1.EnvVar{
		{Name: "CIDR4", Value: config.CIDR.String()},
		{Name: "CIDR6", Value: config.CIDR6.String()},
		{Name: "TrafficManagerService", Value: fmt.Sprintf("kubevpn-traffic-manager.%s", opts.ManagerNamespace)},
		{Name: "POD_UID", Value: string(pod.UID)},
	}, tlsEnv...)

	vpnScript := `
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 0 > /proc/sys/net/ipv4/tcp_timestamps
for b in iptables-nft iptables-legacy; do "$b" -t nat -L &>/dev/null && update-alternatives --set iptables /usr/sbin/"$b" && update-alternatives --set ip6tables /usr/sbin/${b/iptables/ip6tables} && break; done
iptables -P INPUT ACCEPT; ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT; ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING -p tcp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006
exec kubevpn server --debug -l tun:/tcp://${TrafficManagerService}:10801?route=${CIDR4}
`
	// The envoy payload renders the same embedded bootstrap template the mesh
	// envoy sidecar uses.
	envoyScript := `
kubevpn agent-envoy-config --manager ${TrafficManagerService} > /etc/kubevpn/envoy.yaml
exec envoy -c /etc/kubevpn/envoy.yaml --log-level warning
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentJobName(pod.UID),
			Namespace: opts.ManagerNamespace,
			Labels: map[string]string{
				"kubevpn/owner-id": opts.OwnerID,
				"kubevpn/pod-uid":  string(pod.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"kubevpn/owner-id": opts.OwnerID,
					"kubevpn/pod-uid":  string(pod.UID),
				}},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": pod.Spec.NodeName},
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
							Image:   opts.Image,
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{fmt.Sprintf("kubevpn agent --pod-uid %s --proc-root /host/proc -- /bin/bash -c %q", pod.UID, vpnScript)},
							Env:     commonEnv,
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []v1.VolumeMount{{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true}},
						},
						{
							Name:    "envoy",
							Image:   opts.Image,
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{fmt.Sprintf("kubevpn agent --pod-uid %s --proc-root /host/proc -- /bin/bash -c %q", pod.UID, envoyScript)},
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
