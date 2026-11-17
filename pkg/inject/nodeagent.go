package inject

import "context"

// nodeAgentInjector attaches to a workload without patching it: writes the
// same ENVOY_CONFIG rules as mesh mode, then ensures one privileged Job per
// target pod in the manager namespace.
type nodeAgentInjector struct {
	opts InjectOptions
}

func (n *nodeAgentInjector) Inject(ctx context.Context) error {
	return nil // implemented in Task 4
}
