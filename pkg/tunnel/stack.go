package tunnel

import (
	"context"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel/internal/netstack"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// NewLocalStack constructs a localhost-forwarding stack for endpoint integrations.
// packet bridging and lifetime remain with the caller.
func NewLocalStack(ctx context.Context, endpoint stack.LinkEndpoint) *stack.Stack {
	return netstack.NewLocalStack(ctx, endpoint)
}
