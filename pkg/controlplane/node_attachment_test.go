package controlplane

import "testing"

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
