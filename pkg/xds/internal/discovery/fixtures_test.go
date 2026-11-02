package discovery

import (
	"context"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"testing"
	"time"
)

type testServer struct {
	routes    *Broadcaster
	clientset kubernetes.Interface
}
type testEnv struct {
	server *testServer
	client *domainClient
}
type domainClient struct {
	server *testServer
	t      *testing.T
}
type routeRequest struct{ Namespace string }
type routeReceive struct {
	frame *Frame
	err   error
}
type domainStream struct {
	ctx     context.Context
	results chan routeReceive
}

func newTestEnv(t *testing.T) *testEnv {
	cs := fake.NewSimpleClientset()
	s := &testServer{clientset: cs, routes: New(context.Background(), cs)}
	t.Cleanup(func() { s.routes.Close() })
	return &testEnv{server: s, client: &domainClient{server: s, t: t}}
}
func (c *domainClient) WatchNamespaceRoutes(ctx context.Context, req *routeRequest) (*domainStream, error) {
	ctx, cancel := context.WithCancel(ctx)
	stream := &domainStream{ctx: ctx, results: make(chan routeReceive, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ch, unsub, err := c.server.routes.Subscribe(ctx, req.Namespace)
		if err != nil {
			stream.results <- routeReceive{err: err}
			return
		}
		defer unsub()
		for {
			select {
			case frame := <-ch:
				select {
				case stream.results <- routeReceive{frame: frame}:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			case <-c.server.routes.Done():
				stream.results <- routeReceive{err: c.server.routes.Err()}
				return
			}
		}
	}()
	c.t.Cleanup(func() { cancel(); <-done })
	return stream, nil
}
func (s *domainStream) Recv() (*Frame, error) {
	select {
	case r := <-s.results:
		return r.frame, r.err
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func receiveRouteAsync(stream *domainStream) <-chan routeReceive {
	out := make(chan routeReceive, 1)
	go func() { f, e := stream.Recv(); out <- routeReceive{f, e} }()
	return out
}
func awaitRouteReceive(t *testing.T, out <-chan routeReceive, timeout time.Duration) routeReceive {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(timeout):
		t.Fatal("route initialization did not finish within its budget")
		return routeReceive{}
	}
}
func recvRoutes(t *testing.T, stream *domainStream) *Frame {
	t.Helper()
	r := awaitRouteReceive(t, receiveRouteAsync(stream), 5*time.Second)
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.frame
}
func mkSvc(name, clusterIP, externalName string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{ClusterIP: clusterIP, ExternalName: externalName},
	}
}

// recvRoutes reads one frame with a timeout.
func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestIntegration_WatchNamespaceRoutes_SnapshotAndDelta wires the real TunConfigServer +
// gRPC and drives the full lifecycle: snapshot on subscribe, aggregated pod /24 prefixes,
// service records, and delta frames on pod add / service delete. gzip is enabled on the call.
