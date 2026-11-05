package leases

import (
	"context"
	"net"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/envoy"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

type Virtual = controlplane.Virtual
type Rule = controlplane.Rule
type ContainerPort = controlplane.ContainerPort

const CurrentSchemaVersion = controlplane.CurrentSchemaVersion

func parseYaml(content string) ([]*Virtual, error) {
	out := make([]*Virtual, 0)
	err := yaml.Unmarshal([]byte(content), &out)
	return out, err
}

func newManager(ctx context.Context, clientset kubernetes.Interface, ns string) (*Manager, error) {
	rules := envoy.New(clientset, ns)
	return New(ctx, clientset, ns, Callbacks{Sync: rules.Sync, Remove: rules.Remove})
}

func parseIPs(raw []string) []net.IP {
	out := make([]net.IP, 0, len(raw))
	for _, ip := range raw {
		out = append(out, net.ParseIP(ip))
	}
	return out
}

// domainClient owns subscription contexts, without a second RPC mapping implementation.
type domainClient struct {
	*Manager
	t *testing.T
}
type domainStream struct {
	ctx    context.Context
	events chan *Event
	done   chan error
}

func (c *domainClient) WatchTunIP(ctx context.Context, req *Request) (*domainStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream := &domainStream{ctx: ctx, events: make(chan *Event, 8), done: make(chan error, 1)}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		stream.done <- c.Watch(ctx, req.OwnerID, func(event *Event) error {
			select {
			case stream.events <- event:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	c.t.Cleanup(func() { cancel(); <-joined })
	return stream, nil
}
func (s *domainStream) Recv() (*Event, error) {
	select {
	case event := <-s.events:
		return event, nil
	case err := <-s.done:
		return nil, err
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
