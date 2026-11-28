package controlplane

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDesiredVirtuals(t *testing.T) {
	desired := []*Virtual{{Namespace: "default", UID: "deployments.apps.reviews"}}
	if err := ValidateDesiredVirtuals(desired); err != nil {
		t.Fatalf("valid desired state rejected: %v", err)
	}

	mixed := append(desired, &Virtual{
		Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a",
		AgentOrigins: []*AgentOrigin{{PodUID: "pod-a"}},
	})
	err := ValidateDesiredVirtuals(mixed)
	if !errors.Is(err, ErrMixedVirtualState) {
		t.Fatalf("expected ErrMixedVirtualState, got %v", err)
	}
	if !strings.Contains(err.Error(), "reinstall the traffic manager") {
		t.Fatalf("error is not actionable: %v", err)
	}
}
