package xds

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// AttachmentPublisher publishes desired NodeAttachments. It does not create
// Jobs, inspect agent pods, or own any per-pod data-plane lifecycle.
type AttachmentPublisher struct {
	clientset        kubernetes.Interface
	managerNamespace string
	interval         time.Duration
}

func NewAttachmentPublisher(clientset kubernetes.Interface, managerNamespace string) *AttachmentPublisher {
	return &AttachmentPublisher{clientset: clientset, managerNamespace: managerNamespace, interval: 5 * time.Second}
}

func (p *AttachmentPublisher) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if err := p.reconcileOnce(ctx); err != nil {
			plog.G(ctx).Warnf("[AttachmentPublisher] reconcile failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *AttachmentPublisher) reconcileOnce(ctx context.Context) error {
	cm, err := p.clientset.CoreV1().ConfigMaps(p.managerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read traffic manager configmap: %w", err)
	}
	var storedVirtuals []*controlplane.Virtual
	if data := cm.Data[config.KeyEnvoy]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &storedVirtuals); err != nil {
			return fmt.Errorf("parse envoy config: %w", err)
		}
	}
	// Derived per-node Virtuals are regenerated below, never treated as input.
	virtuals := make([]*controlplane.Virtual, 0, len(storedVirtuals))
	for _, virtual := range storedVirtuals {
		if len(virtual.AgentOrigins) == 0 {
			virtuals = append(virtuals, virtual)
		}
	}
	var previous []controlplane.NodeAttachment
	if data := cm.Data[config.KeyNodeAttachments]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &previous); err != nil {
			return fmt.Errorf("parse previous node attachments: %w", err)
		}
	}

	allocations := make(map[string]int32)
	if data := cm.Data[config.KeyAgentPortAllocations]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &allocations); err != nil {
			return fmt.Errorf("parse agent port allocations: %w", err)
		}
	}

	portsChanged, err := controlplane.EnsureAgentListenerPortsWithState(virtuals, allocations)
	if err != nil {
		return fmt.Errorf("allocate agent listener ports: %w", err)
	}
	virtualByKey := make(map[string]*controlplane.Virtual, len(virtuals))
	for _, virtual := range virtuals {
		virtualByKey[virtual.Namespace+"/"+virtual.UID] = virtual
	}

	attachments := make([]controlplane.NodeAttachment, 0)
	for _, virtual := range virtuals {
		if virtual.Selector == "" {
			continue
		}
		agent := false
		for _, rule := range virtual.Rules {
			if rule != nil && rule.IsAgent {
				agent = true
				break
			}
		}
		if !agent {
			continue
		}
		pods, err := p.clientset.CoreV1().Pods(virtual.Namespace).List(ctx, metav1.ListOptions{LabelSelector: virtual.Selector})
		if err != nil {
			plog.G(ctx).Warnf("[AttachmentPublisher] list pods for %s/%s failed, skipping workload: %v", virtual.Namespace, virtual.UID, err)
			continue
		}
		matchedPods := make([]corev1.Pod, 0, len(pods.Items))
		nodes := make(map[string]struct{})
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || pod.Spec.NodeName == "" || podHasKubevpnSidecar(pod) {
				continue
			}
			matchedPods = append(matchedPods, *pod)
			nodes[pod.Spec.NodeName] = struct{}{}
			ports := make([]controlplane.AgentOriginPort, 0, len(virtual.Ports))
			for _, port := range virtual.Ports {
				ports = append(ports, controlplane.AgentOriginPort{ContainerPort: port.ContainerPort, Protocol: port.Protocol})
			}
			attachments = append(attachments, controlplane.NodeAttachment{
				PodUID:      string(pod.UID),
				Namespace:   pod.Namespace,
				NodeName:    pod.Spec.NodeName,
				PodIP:       pod.Status.PodIP,
				WorkloadUID: virtual.UID,
				Ports:       ports,
			})
		}
		serviceAttachments, err := buildServiceAttachments(ctx, p.clientset, virtual, matchedPods, nodes)
		if err != nil {
			return err
		}
		attachments = append(attachments, serviceAttachments...)
	}
	if err := controlplane.EnsureNodeAttachmentPortsWithState(virtuals, previous, attachments, allocations); err != nil {
		return fmt.Errorf("allocate per-pod agent listeners: %w", err)
	}

	derivedByNodeID := make(map[string]*controlplane.Virtual)
	for _, attachment := range attachments {
		virtual := virtualByKey[attachment.Namespace+"/"+attachment.WorkloadUID]
		derived := derivedByNodeID[attachment.NodeID]
		if derived == nil {
			derived = &controlplane.Virtual{
				SchemaVersion:     virtual.SchemaVersion,
				Namespace:         virtual.Namespace,
				UID:               attachment.NodeID,
				AgentListenerPort: virtual.AgentListenerPort,
				Ports:             virtual.Ports,
				Rules:             virtual.Rules,
			}
			derivedByNodeID[attachment.NodeID] = derived
		}
		derived.AgentOrigins = append(derived.AgentOrigins, &controlplane.AgentOrigin{
			PodUID:      attachment.PodUID,
			ServiceName: attachment.ServiceName,
			PodIP:       attachment.PodIP,
			OriginIP:    attachment.OriginIP,
			Ports:       attachment.Ports,
		})
	}
	nodeIDs := make([]string, 0, len(derivedByNodeID))
	for nodeID := range derivedByNodeID {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	updatedVirtuals := append([]*controlplane.Virtual{}, virtuals...)
	for _, nodeID := range nodeIDs {
		updatedVirtuals = append(updatedVirtuals, derivedByNodeID[nodeID])
	}

	envoyYAML, err := yaml.Marshal(updatedVirtuals)
	if err != nil {
		return fmt.Errorf("marshal envoy config: %w", err)
	}
	attachmentsYAML, err := yaml.Marshal(attachments)
	if err != nil {
		return fmt.Errorf("marshal node attachments: %w", err)
	}
	allocationsYAML, err := yaml.Marshal(allocations)
	if err != nil {
		return fmt.Errorf("marshal agent port allocations: %w", err)
	}
	if (portsChanged || cm.Data[config.KeyEnvoy] != string(envoyYAML)) ||
		(cm.Data[config.KeyNodeAttachments] != string(attachmentsYAML) || cm.Data[config.KeyAgentPortAllocations] != string(allocationsYAML)) {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(envoyYAML)
		cm.Data[config.KeyNodeAttachments] = string(attachmentsYAML)
		cm.Data[config.KeyAgentPortAllocations] = string(allocationsYAML)
		if _, err := p.clientset.CoreV1().ConfigMaps(p.managerNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("write node attachments: %w", err)
		}
	}
	return nil
}

// podHasKubevpnSidecar reports whether the pod already carries kubevpn
// sidecar containers. Such pods are served by their own sidecar envoy and
// must not also be attached to the node-boundary agent.
func podHasKubevpnSidecar(pod *corev1.Pod) bool {
	for i := range pod.Spec.Containers {
		name := pod.Spec.Containers[i].Name
		if name == config.ContainerSidecarVPN || name == config.ContainerSidecarEnvoy {
			return true
		}
	}
	return false
}
