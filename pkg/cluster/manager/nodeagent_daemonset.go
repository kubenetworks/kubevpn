package manager

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const nodeAgentDaemonSetName = "kubevpn-nodeagent"

// genNodeAgentDaemonSet returns one node-local agent per node. hostNetwork is
// deliberately disabled: the agent uses its own Pod IP as the DNAT target.
func genNodeAgentDaemonSet(namespace, image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodeAgentDaemonSetName,
			Namespace: namespace,
			Labels:    map[string]string{"app": nodeAgentDaemonSetName},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": nodeAgentDaemonSetName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": nodeAgentDaemonSetName}},
				Spec: corev1.PodSpec{
					HostNetwork: false,
					HostPID:     true,
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"kubevpn", "node-agent"},
						Env: []corev1.EnvVar{
							{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
							{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
							{Name: "MANAGER_NAMESPACE", Value: namespace},
							{Name: "IMAGE", Value: image},
						},
						SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
						VolumeMounts: []corev1.VolumeMount{{
							Name: "host-proc", MountPath: "/host/proc", ReadOnly: true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "host-proc",
						VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: "/proc", Type: ptr.To(corev1.HostPathDirectory),
						}},
					}},
				},
			},
		},
	}
}

// ensureNodeAgentDaemonSet creates or updates the single per-node agent.
func ensureNodeAgentDaemonSet(ctx context.Context, clientset kubernetes.Interface, namespace, image string) error {
	desired := genNodeAgentDaemonSet(namespace, image)
	existing, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, nodeAgentDaemonSetName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = clientset.AppsV1().DaemonSets(namespace).Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create node-agent daemonset: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get node-agent daemonset: %w", err)
	}
	if len(existing.Spec.Template.Spec.Containers) == 1 && existing.Spec.Template.Spec.Containers[0].Image == image {
		return nil
	}
	existing.Spec = desired.Spec
	if _, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update node-agent daemonset: %w", err)
	}
	return nil
}
