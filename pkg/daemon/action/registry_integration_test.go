package action

import (
	"context"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

type registryStatusSession struct{ *handler.ConnectOptions }

func (s *registryStatusSession) GetClusterName() string { return "cluster" }
func (s *registryStatusSession) StatusSnapshot(context.Context) (handler.SessionStatus, error) {
	return handler.SessionStatus{Envoy: `[{"UID":"deployments.apps.web","Namespace":"work","Rules":[{"OwnerID":"alice","Headers":{"version":"test"},"PortMap":{"8080":"30000:9090"}}]}]`, Sync: &handler.SyncStatus{Workloads: []string{"deploy/web"}, Headers: map[string]string{"user": "alice"}, TargetWorkloadNames: map[string]string{"deploy/web": "deploy/web-sync"}, SyncthingGUIAddr: "http://127.0.0.1:8384"}}, nil
}

func TestRegistry_RealGRPCStatusAndDuplicateSelection(t *testing.T) {
	first := &registryStatusSession{&handler.ConnectOptions{ConnectionID: "same", OwnerID: "alice", WorkloadNamespace: "work", OriginKubeconfigPath: "/config", SocksListenAddr: "127.0.0.1:1080", SocksEgress: true}}
	second := &registryStatusSession{&handler.ConnectOptions{ConnectionID: "same", OwnerID: "bob"}}
	svr := &Server{IsSudo: true}
	svr.registry.add(first, true)
	svr.registry.add(second, false)
	cli := recoveryGRPCClient(t, svr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := cli.Status(ctx, &rpc.StatusRequest{ConnectionIDs: []string{"same", "missing", "same"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.List) != 2 || resp.CurrentConnectionID != "same" {
		t.Fatalf("filter changed: %v", resp)
	}
	for _, s := range resp.List {
		if s.Cluster != "cluster" || s.Namespace != "work" || s.Kubeconfig != "/config" || s.SocksListenAddr != "127.0.0.1:1080" || !s.SocksEgress {
			t.Fatalf("identity/SOCKS changed: %v", s)
		}
		if len(s.ProxyList) != 1 || len(s.SyncList) != 1 {
			t.Fatalf("status lists changed: %v", s)
		}
		p := s.ProxyList[0]
		sy := s.SyncList[0]
		if p.Workload != "deployments.apps/web" || !p.RuleList[0].CurrentDevice || p.RuleList[0].PortMap[8080] != 9090 || sy.Workload != "deploy/web" || sy.SyncthingGUIAddr != "http://127.0.0.1:8384" || sy.RuleList[0].DstWorkload != "deploy/web-sync" {
			t.Fatalf("proxy/sync rendering changed: %v", s)
		}
	}
	removed := svr.registry.removeByID("same")
	if len(removed) != 2 || removed[0] != first || removed[1] != second {
		t.Fatal("duplicate removal order changed")
	}
	remaining, current := svr.registry.snapshot()
	if len(remaining) != 0 || current != "" {
		t.Fatal("removal did not clear membership and selection")
	}
}

type registryBlockingPeer struct {
	rpc.UnimplementedDaemonServer
	entered chan struct{}
	release chan struct{}
}

func (p *registryBlockingPeer) Status(ctx context.Context, _ *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	close(p.entered)
	select {
	case <-p.release:
		return &rpc.StatusResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRegistry_ConnectionListPeerRPCDoesNotHoldLock(t *testing.T) {
	peer := &registryBlockingPeer{entered: make(chan struct{}), release: make(chan struct{})}
	client := recoveryGRPCClient(t, peer)
	svr := &Server{GetClient: func(bool) (rpc.DaemonClient, error) { return client, nil }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	}()
	defer func() { close(peer.release); <-done }()
	select {
	case <-peer.entered:
	case <-time.After(time.Second):
		t.Fatal("peer status did not start")
	}
	published := make(chan struct{})
	go func() { svr.registry.add(&handler.ConnectOptions{ConnectionID: "new"}, true); close(published) }()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("peer RPC holds registry lock")
	}
}

func TestRegistry_DisconnectCleanupAllowsReplacement(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	old := &handler.ConnectOptions{ConnectionID: "same"}
	old.AddRollbackFunc(func() error { close(entered); <-release; return nil })
	svr := &Server{IsSudo: true}
	svr.registry.add(old, true)
	go func() { disconnect(context.Background(), svr, "same"); close(done) }()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	replacement := &handler.ConnectOptions{ConnectionID: "same"}
	published := make(chan struct{})
	go func() {
		svr.registry.add(replacement, true)
		// A delayed retirement of the old session must not delete its replacement.
		svr.registry.removeInstance(old)
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("cleanup holds registry lock")
	}
	got, _ := svr.registry.find("same")
	if got != replacement {
		t.Fatal("replacement removed by stale instance")
	}
	if _, err := svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{ConnectionID: "same"}); err != nil {
		t.Fatal(err)
	}
	snapshot, current := svr.registry.snapshot()
	if len(snapshot) != 1 || current != "same" {
		t.Fatalf("invalid registry snapshot: %v %q", snapshot, current)
	}
	snapshot[0] = old
	got, _ = svr.registry.find("same")
	if got != replacement {
		t.Fatal("snapshot aliases registry storage")
	}
}
