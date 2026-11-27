package controlplane

import (
	"fmt"
	"sort"
)

const (
	AgentListenerPortMin int32 = 20000
	AgentListenerPortMax int32 = 24999
)

// NodeAttachment is the desired attachment of one target pod to the agent on
// its node. It is declarative desired state, not a per-pod process/lease record.
type NodeAttachment struct {
	PodUID      string `json:"podUID" yaml:"podUID"`
	Namespace   string `json:"namespace" yaml:"namespace"`
	NodeName    string `json:"nodeName" yaml:"nodeName"`
	PodIP       string `json:"podIP" yaml:"podIP"`
	WorkloadUID string `json:"workloadUID" yaml:"workloadUID"`
	// ServiceName marks a ClusterIP attachment; empty means a real pod attachment.
	ServiceName string `json:"serviceName,omitempty" yaml:"serviceName,omitempty"`
	// OriginIP is the explicit return destination. Empty means PodIP.
	OriginIP string `json:"originIP,omitempty" yaml:"originIP,omitempty"`
	// NodeID identifies the manager-derived Virtual for this workload on this node.
	NodeID            string            `json:"nodeID" yaml:"nodeID"`
	AgentListenerPort int32             `json:"agentListenerPort" yaml:"agentListenerPort"`
	Ports             []AgentOriginPort `json:"ports" yaml:"ports"`
}

// NodeBoundaryNodeID returns the derived xDS identity shared by all pods of a
// workload on one node. It is not a per-pod lease or process identity.
func NodeBoundaryNodeID(workloadUID, nodeName string) string {
	return fmt.Sprintf("%s.nodes.%s", workloadUID, nodeName)
}

// EnsureNodeAttachmentPorts assigns stable direct listeners without
// persisting tombstones. New control-plane callers should use
// EnsureNodeAttachmentPortsWithState so removed origins cannot be reused by a
// different workload while their old Envoy is still running.
func EnsureNodeAttachmentPorts(virtuals []*Virtual, previous, attachments []NodeAttachment) error {
	return EnsureNodeAttachmentPortsWithState(virtuals, previous, attachments, nil)
}

func agentWorkloadPortKey(virtual *Virtual) string {
	return "workload/" + virtual.Namespace + "/" + virtual.UID
}

func agentOriginPortKey(attachment NodeAttachment, port AgentOriginPort) string {
	return fmt.Sprintf("origin/%s/%s/%s/%s/%d/%d", attachment.Namespace, attachment.WorkloadUID, attachment.PodUID, port.Protocol, port.ContainerPort, port.DestinationPort())
}

// EnsureNodeAttachmentPortsWithState assigns stable, globally unique direct
// listeners and preserves historical assignments in allocations. A removed
// origin keeps its tombstone until the manager ConfigMap itself is deleted.
func EnsureNodeAttachmentPortsWithState(virtuals []*Virtual, previous, attachments []NodeAttachment, allocations map[string]int32) error {
	virtualByKey := make(map[string]*Virtual, len(virtuals))
	used := make(map[int32]string)
	for key, port := range allocations {
		used[port] = key
	}
	for _, virtual := range virtuals {
		virtualByKey[virtual.Namespace+"/"+virtual.UID] = virtual
		if virtual.AgentListenerPort != 0 {
			used[virtual.AgentListenerPort] = agentWorkloadPortKey(virtual)
		}
	}
	previousPorts := make(map[string]int32)
	for _, attachment := range previous {
		for _, port := range attachment.Ports {
			key := agentOriginPortKey(attachment, port)
			previousPorts[key] = port.AgentListenerPort
			used[port.AgentListenerPort] = key
			if allocations != nil {
				allocations[key] = port.AgentListenerPort
			}
		}
	}
	sort.Slice(attachments, func(i, j int) bool {
		a, b := attachments[i], attachments[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.WorkloadUID != b.WorkloadUID {
			return a.WorkloadUID < b.WorkloadUID
		}
		if a.NodeName != b.NodeName {
			return a.NodeName < b.NodeName
		}
		return a.PodUID < b.PodUID
	})
	next := AgentListenerPortMin
	allocate := func() (int32, error) {
		for ; next <= AgentListenerPortMax; next++ {
			if used[next] == "" {
				port := next
				next++
				return port, nil
			}
		}
		return 0, fmt.Errorf("no available agent listener ports")
	}
	for i := range attachments {
		attachment := &attachments[i]
		virtual := virtualByKey[attachment.Namespace+"/"+attachment.WorkloadUID]
		if virtual == nil {
			return fmt.Errorf("virtual for attachment %s/%s not found", attachment.Namespace, attachment.WorkloadUID)
		}
		attachment.NodeID = NodeBoundaryNodeID(attachment.WorkloadUID, attachment.NodeName)
		attachment.AgentListenerPort = virtual.AgentListenerPort
		for j := range attachment.Ports {
			port := &attachment.Ports[j]
			key := agentOriginPortKey(*attachment, *port)
			assigned := previousPorts[key]
			if assigned == 0 {
				assigned = allocations[key]
			}
			owner := used[assigned]
			if assigned < AgentListenerPortMin || assigned > AgentListenerPortMax || (owner != "" && owner != key) {
				var err error
				assigned, err = allocate()
				if err != nil {
					return err
				}
			}
			port.AgentListenerPort = assigned
			used[assigned] = key
			if allocations != nil {
				allocations[key] = assigned
			}
		}
	}
	return nil
}

// EnsureAgentListenerPorts assigns stable, unique listener ports to agent
// Virtuals without persisting tombstones.
func EnsureAgentListenerPorts(virtuals []*Virtual) (bool, error) {
	return EnsureAgentListenerPortsWithState(virtuals, nil)
}

// EnsureAgentListenerPortsWithState assigns stable, unique base ports and
// preserves historical assignments so a removed workload cannot donate its
// admin port to another live Envoy.
func EnsureAgentListenerPortsWithState(virtuals []*Virtual, allocations map[string]int32) (bool, error) {
	used := make(map[int32]string)
	for key, port := range allocations {
		used[port] = key
	}
	next := AgentListenerPortMin
	allocate := func() (int32, error) {
		for ; next <= AgentListenerPortMax; next++ {
			if used[next] == "" {
				port := next
				next++
				return port, nil
			}
		}
		return 0, fmt.Errorf("no available agent listener ports")
	}
	changed := false
	for _, virtual := range virtuals {
		agent := false
		for _, rule := range virtual.Rules {
			if rule != nil && rule.IsAgent {
				agent = true
				break
			}
		}
		if virtual.Selector == "" || !agent {
			continue
		}
		key := agentWorkloadPortKey(virtual)
		if virtual.AgentListenerPort == 0 && allocations[key] != 0 {
			virtual.AgentListenerPort = allocations[key]
			changed = true
		}
		if virtual.AgentListenerPort != 0 {
			owner := used[virtual.AgentListenerPort]
			if owner == "" || owner == key {
				used[virtual.AgentListenerPort] = key
				if allocations != nil {
					allocations[key] = virtual.AgentListenerPort
				}
				continue
			}
		}
		port, err := allocate()
		if err != nil {
			return changed, err
		}
		virtual.AgentListenerPort = port
		used[port] = key
		if allocations != nil {
			allocations[key] = port
		}
		changed = true
	}
	return changed, nil
}
