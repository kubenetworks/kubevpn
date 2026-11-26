package controlplane

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEnsureAgentListenerPorts_UniqueAndStable(t *testing.T) {
	virtuals := []*Virtual{
		{UID: "deployments.apps.reviews", Selector: "app=reviews", Rules: []*Rule{{IsAgent: true}}},
		{UID: "deployments.apps.details", Selector: "app=details", Rules: []*Rule{{IsAgent: true}}},
		{UID: "deployments.apps.old", Selector: "app=old", Rules: []*Rule{{IsAgent: true}}, AgentListenerPort: 20100},
	}
	changed, err := EnsureAgentListenerPorts(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected new ports to be assigned")
	}
	seen := map[int32]bool{}
	for _, v := range virtuals {
		if v.AgentListenerPort < AgentListenerPortMin || v.AgentListenerPort > AgentListenerPortMax {
			t.Fatalf("port out of range: %d", v.AgentListenerPort)
		}
		if seen[v.AgentListenerPort] {
			t.Fatalf("duplicate port: %d", v.AgentListenerPort)
		}
		seen[v.AgentListenerPort] = true
	}
	if virtuals[2].AgentListenerPort != 20100 {
		t.Fatal("existing port changed")
	}
	changed, err = EnsureAgentListenerPorts(virtuals)
	if err != nil || changed {
		t.Fatalf("second allocation must be stable: changed=%v err=%v", changed, err)
	}
}

func TestEnsureAgentListenerPorts_SkipsSidecar(t *testing.T) {
	v := &Virtual{UID: "deployments.apps.reviews", Rules: []*Rule{{IsAgent: false}}}
	changed, err := EnsureAgentListenerPorts([]*Virtual{v})
	if err != nil || changed || v.AgentListenerPort != 0 {
		t.Fatalf("sidecar virtual changed: changed=%v err=%v port=%d", changed, err, v.AgentListenerPort)
	}
}

func TestEnsureNodeAttachmentPorts_UniqueAndStable(t *testing.T) {
	virtuals := []*Virtual{{
		Namespace: "default", UID: "deployments.apps.reviews", AgentListenerPort: 20000,
	}}
	ports := func() []AgentOriginPort {
		return []AgentOriginPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
			{ContainerPort: 9081, Protocol: corev1.ProtocolUDP},
		}
	}
	attachments := []NodeAttachment{
		{Namespace: "default", WorkloadUID: "deployments.apps.reviews", NodeName: "node-a", PodUID: "pod-b", PodIP: "10.244.0.6", Ports: ports()},
		{Namespace: "default", WorkloadUID: "deployments.apps.reviews", NodeName: "node-a", PodUID: "pod-a", PodIP: "10.244.0.5", Ports: ports()},
	}
	if err := EnsureNodeAttachmentPorts(virtuals, nil, attachments); err != nil {
		t.Fatal(err)
	}
	used := map[int32]string{20000: "workload-admin"}
	previous := make(map[string]int32)
	for _, attachment := range attachments {
		if attachment.NodeID != NodeBoundaryNodeID(attachment.WorkloadUID, "node-a") {
			t.Fatalf("unexpected derived node ID: %s", attachment.NodeID)
		}
		if attachment.AgentListenerPort != 20000 {
			t.Fatalf("admin base port changed: %d", attachment.AgentListenerPort)
		}
		for _, port := range attachment.Ports {
			if owner, exists := used[port.AgentListenerPort]; exists {
				t.Fatalf("port %d shared by %s and %s", port.AgentListenerPort, owner, attachment.PodUID)
			}
			used[port.AgentListenerPort] = attachment.PodUID
			previous[fmt.Sprintf("%s/%s/%d", attachment.PodUID, port.Protocol, port.ContainerPort)] = port.AgentListenerPort
		}
	}

	reordered := []NodeAttachment{attachments[1], attachments[0]}
	if err := EnsureNodeAttachmentPorts(virtuals, attachments, reordered); err != nil {
		t.Fatal(err)
	}
	for _, attachment := range reordered {
		for _, port := range attachment.Ports {
			key := fmt.Sprintf("%s/%s/%d", attachment.PodUID, port.Protocol, port.ContainerPort)
			if previous[key] != port.AgentListenerPort {
				t.Fatalf("port changed for %s: got %d want %d", key, port.AgentListenerPort, previous[key])
			}
		}
	}
}

func TestEnsureAgentListenerPortsWithState_DoesNotReuseRemovedWorkload(t *testing.T) {
	allocations := map[string]int32{}
	reviews := &Virtual{Namespace: "default", UID: "reviews", Selector: "app=reviews", Rules: []*Rule{{IsAgent: true}}}
	changed, err := EnsureAgentListenerPortsWithState([]*Virtual{reviews}, allocations)
	if err != nil || !changed {
		t.Fatalf("initial allocation failed: changed=%v err=%v", changed, err)
	}
	authors := &Virtual{Namespace: "default", UID: "authors", Selector: "app=authors", Rules: []*Rule{{IsAgent: true}}}
	if _, err := EnsureAgentListenerPortsWithState([]*Virtual{authors}, allocations); err != nil {
		t.Fatal(err)
	}
	if authors.AgentListenerPort == reviews.AgentListenerPort {
		t.Fatalf("removed workload port was reused while its Envoy may still run: %d", authors.AgentListenerPort)
	}
	readded := &Virtual{Namespace: "default", UID: "reviews", Selector: "app=reviews", Rules: []*Rule{{IsAgent: true}}}
	if _, err := EnsureAgentListenerPortsWithState([]*Virtual{readded}, allocations); err != nil {
		t.Fatal(err)
	}
	if readded.AgentListenerPort != reviews.AgentListenerPort {
		t.Fatalf("re-added workload did not recover its stable port: got %d want %d", readded.AgentListenerPort, reviews.AgentListenerPort)
	}
}

func TestEnsureNodeAttachmentPortsWithState_DoesNotReuseRemovedOrigin(t *testing.T) {
	allocations := map[string]int32{}
	virtuals := []*Virtual{
		{Namespace: "default", UID: "reviews", AgentListenerPort: 20000},
		{Namespace: "default", UID: "authors", AgentListenerPort: 20001},
	}
	ports := func() []AgentOriginPort {
		{
			return []AgentOriginPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}}
		}
	}
	reviews := []NodeAttachment{{Namespace: "default", WorkloadUID: "reviews", NodeName: "node-a", PodUID: "reviews-pod", PodIP: "10.244.0.5", Ports: ports()}}
	if err := EnsureNodeAttachmentPortsWithState(virtuals, nil, reviews, allocations); err != nil {
		t.Fatal(err)
	}
	authors := []NodeAttachment{{Namespace: "default", WorkloadUID: "authors", NodeName: "node-a", PodUID: "authors-pod", PodIP: "10.244.0.7", Ports: ports()}}
	if err := EnsureNodeAttachmentPortsWithState(virtuals, reviews, authors, allocations); err != nil {
		t.Fatal(err)
	}
	if authors[0].Ports[0].AgentListenerPort == reviews[0].Ports[0].AgentListenerPort {
		t.Fatal("removed pod listener was reused by another workload")
	}
	readded := []NodeAttachment{{Namespace: "default", WorkloadUID: "reviews", NodeName: "node-a", PodUID: "reviews-pod", PodIP: "10.244.0.5", Ports: ports()}}
	if err := EnsureNodeAttachmentPortsWithState(virtuals, reviews, readded, allocations); err != nil {
		t.Fatal(err)
	}
	if readded[0].Ports[0].AgentListenerPort != reviews[0].Ports[0].AgentListenerPort {
		t.Fatalf("re-added pod did not recover its listener: got %d want %d", readded[0].Ports[0].AgentListenerPort, reviews[0].Ports[0].AgentListenerPort)
	}
}

func TestEnsureAgentListenerPorts_Exhaustion(t *testing.T) {
	var virtuals []*Virtual
	for i := AgentListenerPortMin; i <= AgentListenerPortMax; i++ {
		virtuals = append(virtuals, &Virtual{Selector: "app=x", Rules: []*Rule{{IsAgent: true}}, AgentListenerPort: i})
	}
	virtuals = append(virtuals, &Virtual{Selector: "app=new", Rules: []*Rule{{IsAgent: true}}})
	if _, err := EnsureAgentListenerPorts(virtuals); err == nil {
		t.Fatal("expected exhaustion error")
	}
}
