package xds

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// buildServiceAttachments projects Services targeting this workload into every
// node that has an attached pod. ClusterIP/ServicePort remain the return
// destination for unmatched traffic, preserving kube-proxy load balancing.
func buildServiceAttachments(ctx context.Context, clientset kubernetes.Interface, virtual *controlplane.Virtual, pods []corev1.Pod, nodes map[string]struct{}) ([]controlplane.NodeAttachment, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	services, err := clientset.CoreV1().Services(virtual.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services for %s/%s: %w", virtual.Namespace, virtual.UID, err)
	}
	nodeNames := make([]string, 0, len(nodes))
	for nodeName := range nodes {
		nodeNames = append(nodeNames, nodeName)
	}
	sort.Strings(nodeNames)

	var attachments []controlplane.NodeAttachment
	for i := range services.Items {
		service := &services.Items[i]
		if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone || len(service.Spec.Selector) == 0 {
			continue
		}
		selector := labels.SelectorFromSet(service.Spec.Selector)
		matches := false
		for j := range pods {
			if selector.Matches(labels.Set(pods[j].Labels)) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		ports := serviceOriginPorts(service, virtual.Ports)
		if len(ports) == 0 {
			continue
		}
		for _, nodeName := range nodeNames {
			attachments = append(attachments, controlplane.NodeAttachment{
				PodUID:      fmt.Sprintf("service/%s/%s", service.Namespace, service.Name),
				Namespace:   virtual.Namespace,
				ServiceName: service.Name,
				NodeName:    nodeName,
				PodIP:       service.Spec.ClusterIP,
				OriginIP:    service.Spec.ClusterIP,
				WorkloadUID: virtual.UID,
				Ports:       append([]controlplane.AgentOriginPort(nil), ports...),
			})
		}
	}
	return attachments, nil
}

func serviceOriginPorts(service *corev1.Service, virtualPorts []controlplane.ContainerPort) []controlplane.AgentOriginPort {
	var result []controlplane.AgentOriginPort
	for _, servicePort := range service.Spec.Ports {
		protocol := servicePort.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		targetPort := servicePort.TargetPort.IntValue()
		if targetPort == 0 {
			if servicePort.TargetPort.StrVal != "" {
				// A named target must resolve to a declared container port;
				// never silently replace it with the ServicePort.
				targetPort = -1
			} else {
				targetPort = int(servicePort.Port)
			}
		}
		for _, virtualPort := range virtualPorts {
			if servicePort.TargetPort.StrVal != "" && virtualPort.Name == servicePort.TargetPort.StrVal {
				targetPort = int(virtualPort.ContainerPort)
			}
			virtualProtocol := virtualPort.Protocol
			if virtualProtocol == "" {
				virtualProtocol = corev1.ProtocolTCP
			}
			if virtualPort.ContainerPort == int32(targetPort) && virtualProtocol == protocol {
				result = append(result, controlplane.AgentOriginPort{
					ContainerPort: virtualPort.ContainerPort,
					OriginPort:    servicePort.Port,
					Protocol:      protocol,
				})
				break
			}
		}
	}
	return result
}
