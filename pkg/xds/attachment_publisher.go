package xds

import (
	"context"
	"fmt"
	"maps"
	"net"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
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
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		return p.reconcileAttempt(ctx)
	})
}

func (p *AttachmentPublisher) reconcileAttempt(ctx context.Context) error {
	cm, err := p.clientset.CoreV1().ConfigMaps(p.managerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read traffic manager configmap: %w", err)
	}
	var virtuals []*controlplane.Virtual
	if data := cm.Data[config.KeyEnvoy]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &virtuals); err != nil {
			return fmt.Errorf("parse desired envoy config: %w", err)
		}
	}
	if err := controlplane.ValidateDesiredVirtuals(virtuals); err != nil {
		return err
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

	if _, err = controlplane.EnsureAgentListenerPortsWithState(virtuals, allocations); err != nil {
		return fmt.Errorf("allocate agent listener ports: %w", err)
	}
	virtualByKey := make(map[string]*controlplane.Virtual, len(virtuals))
	for _, virtual := range virtuals {
		virtualByKey[virtual.Namespace+"/"+virtual.UID] = virtual
	}

	attachments := make([]controlplane.NodeAttachment, 0)
	explicitServices := make(map[string]struct{})
	for _, virtual := range virtuals {
		if virtual.ServiceName != "" {
			explicitServices[virtual.Namespace+"/"+virtual.ServiceName] = struct{}{}
		}
	}
	syncEndpoints := make(map[*controlplane.Rule]syncEndpoint)
	for _, virtual := range virtuals {
		for _, rule := range virtual.Rules {
			if rule == nil || rule.SyncTarget == nil {
				continue
			}
			endpoint, err := resolveSyncTarget(ctx, p.clientset, virtual.Namespace, rule.SyncTarget)
			if err != nil {
				return err
			}
			syncEndpoints[rule] = endpoint
		}
	}
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
			if virtual.ServiceName == "" {
				attachments = append(attachments, controlplane.NodeAttachment{
					PodUID:      string(pod.UID),
					Namespace:   pod.Namespace,
					NodeName:    pod.Spec.NodeName,
					PodIP:       pod.Status.PodIP,
					WorkloadUID: virtual.UID,
					Ports:       ports,
				})
			}
		}
		serviceAttachments, err := buildServiceAttachments(ctx, p.clientset, virtual, matchedPods, nodes, explicitServices)
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
				Rules:             cloneRulesForDerived(virtual.Rules, syncEndpoints),
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
	if err := validateDerivedUDPRoutes(derivedByNodeID); err != nil {
		return err
	}
	nodeIDs := make([]string, 0, len(derivedByNodeID))
	for nodeID := range derivedByNodeID {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	derivedVirtuals := make([]*controlplane.Virtual, 0, len(derivedByNodeID))
	for _, nodeID := range nodeIDs {
		derivedVirtuals = append(derivedVirtuals, derivedByNodeID[nodeID])
	}

	derivedYAML, err := yaml.Marshal(derivedVirtuals)
	if err != nil {
		return fmt.Errorf("marshal derived xds config: %w", err)
	}
	attachmentsYAML, err := yaml.Marshal(attachments)
	if err != nil {
		return fmt.Errorf("marshal node attachments: %w", err)
	}
	allocationsYAML, err := yaml.Marshal(allocations)
	if err != nil {
		return fmt.Errorf("marshal agent port allocations: %w", err)
	}
	if cm.Data[config.KeyAgentXDS] != string(derivedYAML) ||
		cm.Data[config.KeyNodeAttachments] != string(attachmentsYAML) ||
		cm.Data[config.KeyAgentPortAllocations] != string(allocationsYAML) {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyAgentXDS] = string(derivedYAML)
		cm.Data[config.KeyNodeAttachments] = string(attachmentsYAML)
		cm.Data[config.KeyAgentPortAllocations] = string(allocationsYAML)
		if _, err := p.clientset.CoreV1().ConfigMaps(p.managerNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("write derived node-agent state: %w", err)
		}
	}
	return nil
}

func validateDerivedUDPRoutes(virtuals map[string]*controlplane.Virtual) error {
	for _, virtual := range virtuals {
		for _, origin := range virtual.AgentOrigins {
			for _, port := range origin.Ports {
				if port.Protocol != corev1.ProtocolUDP {
					continue
				}
				var ipv4Routes, ipv6Routes int
				for _, rule := range virtual.Rules {
					if rule.LocalTunIPv4 != "" {
						ipv4Routes++
					}
					if rule.LocalTunIPv6 != "" {
						ipv6Routes++
					}
				}
				if ipv4Routes > 1 || ipv6Routes > 1 {
					return fmt.Errorf("multiple UDP routes for %s/%s listener %d", virtual.Namespace, virtual.UID, port.AgentListenerPort)
				}
			}
		}
	}
	return nil
}

type syncEndpoint struct {
	ipv4 string
	ipv6 string
}

func resolveSyncTarget(ctx context.Context, clientset kubernetes.Interface, namespace string, target *controlplane.SyncTarget) (syncEndpoint, error) {
	var endpoint syncEndpoint
	if target.Selector == "" {
		return endpoint, fmt.Errorf("sync target %s has an empty selector", target.UID)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: target.Selector})
	if err != nil {
		return endpoint, fmt.Errorf("list sync target %s pods: %w", target.UID, err)
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || !podIsReady(pod) {
			continue
		}
		if selected == nil || pod.CreationTimestamp.Time.After(selected.CreationTimestamp.Time) ||
			(pod.CreationTimestamp.Time.Equal(selected.CreationTimestamp.Time) && string(pod.UID) > string(selected.UID)) {
			selected = pod
		}
	}
	if selected == nil {
		return endpoint, nil
	}
	ips := append([]corev1.PodIP{{IP: selected.Status.PodIP}}, selected.Status.PodIPs...)
	for _, podIP := range ips {
		ip := net.ParseIP(podIP.IP)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			endpoint.ipv4 = podIP.IP
		} else {
			endpoint.ipv6 = podIP.IP
		}
	}
	return endpoint, nil
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func cloneRulesForDerived(rules []*controlplane.Rule, endpoints map[*controlplane.Rule]syncEndpoint) []*controlplane.Rule {
	cloned := make([]*controlplane.Rule, 0, len(rules))
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		copyRule := *rule
		copyRule.Headers = maps.Clone(rule.Headers)
		copyRule.PortMap = maps.Clone(rule.PortMap)
		if rule.SyncTarget != nil {
			target := *rule.SyncTarget
			copyRule.SyncTarget = &target
			endpoint := endpoints[rule]
			copyRule.LocalTunIPv4 = endpoint.ipv4
			copyRule.LocalTunIPv6 = endpoint.ipv6
		}
		cloned = append(cloned, &copyRule)
	}
	return cloned
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
