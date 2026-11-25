package controlplane

import "fmt"

const (
	AgentListenerPortMin int32 = 20000
	AgentListenerPortMax int32 = 24999
)

// NodeAttachment is the desired attachment of one target pod to the agent on
// its node. It is declarative desired state, not a per-pod process/lease record.
type NodeAttachment struct {
	PodUID            string `json:"podUID" yaml:"podUID"`
	Namespace         string `json:"namespace" yaml:"namespace"`
	NodeName          string `json:"nodeName" yaml:"nodeName"`
	PodIP             string `json:"podIP" yaml:"podIP"`
	WorkloadUID       string `json:"workloadUID" yaml:"workloadUID"`
	AgentListenerPort int32  `json:"agentListenerPort" yaml:"agentListenerPort"`
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
