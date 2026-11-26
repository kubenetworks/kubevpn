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

// EnsureNodeAttachmentPorts assigns a stable, globally unique direct listener
// to every pod/container port. The workload base port is reserved for the
// Envoy admin listener and is never reused as a data listener.
func EnsureNodeAttachmentPorts(virtuals []*Virtual, previous, attachments []NodeAttachment) error {
	virtualByKey := make(map[string]*Virtual, len(virtuals))
	used := make(map[int32]bool)
	for _, virtual := range virtuals {
		virtualByKey[virtual.Namespace+"/"+virtual.UID] = virtual
		if virtual.AgentListenerPort != 0 {
			used[virtual.AgentListenerPort] = true
		}
	}
	portKey := func(attachment NodeAttachment, port AgentOriginPort) string {
		return fmt.Sprintf("%s/%s/%s/%s/%d", attachment.Namespace, attachment.WorkloadUID, attachment.PodUID, port.Protocol, port.ContainerPort)
	}
	previousPorts := make(map[string]int32)
	for _, attachment := range previous {
		for _, port := range attachment.Ports {
			previousPorts[portKey(attachment, port)] = port.AgentListenerPort
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
			if !used[next] {
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
			assigned := previousPorts[portKey(*attachment, *port)]
			if assigned < AgentListenerPortMin || assigned > AgentListenerPortMax || used[assigned] {
				var err error
				assigned, err = allocate()
				if err != nil {
					return err
				}
			}
			port.AgentListenerPort = assigned
			used[assigned] = true
		}
	}
	return nil
}

// EnsureAgentListenerPorts assigns stable, unique listener ports to agent
// Virtuals. It never changes a valid existing assignment.
func EnsureAgentListenerPorts(virtuals []*Virtual) (bool, error) {
	used := make(map[int32]bool)
	for _, v := range virtuals {
		if v.AgentListenerPort != 0 {
			used[v.AgentListenerPort] = true
		}
	}
	changed := false
	for _, v := range virtuals {
		agent := false
		for _, rule := range v.Rules {
			if rule != nil && rule.IsAgent {
				agent = true
				break
			}
		}
		if v.Selector == "" || !agent {
			continue
		}
		if v.AgentListenerPort != 0 {
			continue
		}
		var port int32
		for port = AgentListenerPortMin; port <= AgentListenerPortMax; port++ {
			if !used[port] {
				break
			}
		}
		if port > AgentListenerPortMax {
			return changed, fmt.Errorf("no available agent listener ports")
		}
		v.AgentListenerPort = port
		used[port] = true
		changed = true
	}
	return changed, nil
}
