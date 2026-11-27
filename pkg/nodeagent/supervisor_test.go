package nodeagent

import "testing"

func TestEnvoyLaunchSpec_UniquePortsAndBaseID(t *testing.T) {
	a := envoyLaunchSpec("deployments.apps.reviews", 20001)
	b := envoyLaunchSpec("deployments.apps.details", 20002)
	if a.adminPort != 25001 || b.adminPort != 25002 {
		t.Fatalf("unexpected admin ports: %d %d", a.adminPort, b.adminPort)
	}
	if a.baseID == b.baseID {
		t.Fatal("multiple Envoys must not share base-id")
	}
	if a.listenerPort != 20001 || b.listenerPort != 20002 {
		t.Fatal("listener port not preserved")
	}
	if a.baseID != 2 || b.baseID != 3 {
		t.Fatalf("unexpected base IDs: %d %d", a.baseID, b.baseID)
	}
}
