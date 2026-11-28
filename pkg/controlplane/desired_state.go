package controlplane

import (
	"errors"
	"fmt"
)

var ErrMixedVirtualState = errors.New("mixed desired and derived virtual state")

// ValidateDesiredVirtuals rejects the pre-separation representation rather than
// silently treating runtime per-node projections as desired input.
func ValidateDesiredVirtuals(virtuals []*Virtual) error {
	for _, virtual := range virtuals {
		if virtual != nil && len(virtual.AgentOrigins) != 0 {
			return fmt.Errorf("%w at %s/%s: reinstall the traffic manager", ErrMixedVirtualState, virtual.Namespace, virtual.UID)
		}
	}
	return nil
}
