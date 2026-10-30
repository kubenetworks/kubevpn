package action

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"gopkg.in/natefinch/lumberjack.v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// The real user handler resolves identity through an HTTP API backed by fake K8s.
// Its retained factory deliberately uses a different endpoint: recovery must not
// depend on the newly created, temporary SSH/API-server tunnel.
func recoveryUserFixture(t *testing.T, cli rpc.DaemonClient) (*Server, *handler.ConnectOptions, *rpc.ConnectRequest) {
	t.Helper()
	cs := fake.NewSimpleClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "00000000-0000-0000-0000-abc123def456"}})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ns, err := cs.CoreV1().Namespaces().Get(r.Context(), "default", metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			http.Error(w, "namespace", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ns)
	}))
	t.Cleanup(api.Close)
	kubeconfig := func(endpoint string) string {
		return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: %s
contexts:
- name: ctx
  context:
    cluster: cluster
    user: user
    namespace: default
current-context: ctx
users:
- name: user
  user:
    token: retained-credential
`, endpoint)
	}
	req := &rpc.ConnectRequest{Namespace: "default", ManagerNamespace: "default", KubeconfigBytes: kubeconfig(api.URL), OwnerID: "temporary-owner"}
	original := proto.Clone(req).(*rpc.ConnectRequest)
	original.OwnerID = "original-owner"
	original.ExtraRoute = &rpc.ExtraRoute{ExtraCIDR: []string{"10.12.0.0/16"}}
	raw, err := proto.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	old := handler.NewConnectOptionsForTest(util.InitFactoryByBytes([]byte(kubeconfig("https://127.0.0.1:32123")), "default"), "retained-config", "default")
	old.ConnectionID = "abc123def456"
	old.ManagerNamespace = "default"
	old.OwnerID = "original-owner"
	old.RequestRaw = raw
	old.SetSync(&handler.SyncOptions{Workloads: []string{"deployment/web"}})
	logFile := &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "daemon.log")}
	t.Cleanup(func() { _ = logFile.Close() })
	svr := &Server{LogFile: logFile, GetClient: func(bool) (rpc.DaemonClient, error) { return cli, nil }, connections: []handler.Connection{old, &handler.ConnectOptions{ConnectionID: "other"}}}
	// A cached healthy result must never mask the fresh root verdict.
	svr.sudoHealth = map[string]tunIP{old.ConnectionID: {status: StatusOk}}
	return svr, old, req
}

type recoveryPeer struct {
	rpc.UnimplementedDaemonServer
	mu          sync.Mutex
	state       string
	statusErr   error
	connectErr  error
	requests    []*rpc.ConnectRequest
	statusCalls int
	bounded     bool
}

func (p *recoveryPeer) Status(ctx context.Context, req *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statusCalls++
	_, p.bounded = ctx.Deadline()
	if p.statusErr != nil {
		return nil, p.statusErr
	}
	if p.state == "absent" {
		return &rpc.StatusResponse{}, nil
	}
	return &rpc.StatusResponse{List: []*rpc.Status{{ConnectionID: "abc123def456", Status: p.state}}}, nil
}
func (p *recoveryPeer) Connect(stream rpc.Daemon_ConnectServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.requests = append(p.requests, req)
	if p.connectErr != nil {
		err := p.connectErr
		p.mu.Unlock()
		return err
	}
	p.state = StatusOk
	p.mu.Unlock()
	return stream.Send(&rpc.ConnectResponse{ConnectionID: req.ConnectionID})
}
func recoveryGRPCClient(t *testing.T, server rpc.DaemonServer) rpc.DaemonClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	g := grpc.NewServer()
	rpc.RegisterDaemonServer(g, server)
	go func() { _ = g.Serve(listener) }()
	cc, err := grpc.NewClient("passthrough:///recovery", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close(); g.Stop(); _ = listener.Close() })
	return rpc.NewDaemonClient(cc)
}

func TestConnectRecovery_UserRetainsControlAndOriginalEndpoint(t *testing.T) {
	for _, initial := range []string{StatusFailed, "absent", StatusOk, StatusUnhealthy} {
		t.Run(initial, func(t *testing.T) {
			peer := &recoveryPeer{state: initial}
			svr, old, req := recoveryUserFixture(t, recoveryGRPCClient(t, peer))
			originalSync := old.GetSync()
			other := svr.connections[1]
			lifecycle := NewSessionLifecycle(logrus.New())
			defer lifecycle.Teardown()
			old.AddRollbackFunc(func() error { lifecycle.Teardown(); return nil })
			for i := 0; i < 2; i++ {
				stream := &fakeConnectServer{ctx: context.Background(), req: req}
				if err := svr.redirectConnectToSudoDaemon(proto.Clone(req).(*rpc.ConnectRequest), stream, logrus.New()); err != nil {
					t.Fatal(err)
				}
				if len(svr.connections) != 2 || svr.connections[0] != old || svr.connections[1] != other || old.GetSync() != originalSync || lifecycle.Ctx.Err() != nil {
					t.Fatal("recovery replaced/cleaned control metadata, SSH lifecycle, or sibling")
				}
			}
			peer.mu.Lock()
			defer peer.mu.Unlock()
			if peer.statusCalls != 2 || !peer.bounded {
				t.Fatalf("expected fresh bounded root queries, calls=%d bounded=%v", peer.statusCalls, peer.bounded)
			}
			want := 0
			if initial == StatusFailed || initial == "absent" {
				want = 1
			}
			if len(peer.requests) != want {
				t.Fatalf("root Connect calls=%d, want %d", len(peer.requests), want)
			}
			if want != 0 {
				forwarded := peer.requests[0]
				rest, err := util.InitFactoryByBytes([]byte(forwarded.KubeconfigBytes), "default").ToRESTConfig()
				if err != nil {
					t.Fatal(err)
				}
				if rest.Host != "https://127.0.0.1:32123" || rest.BearerToken != "retained-credential" || forwarded.OwnerID != old.OwnerID || forwarded.ConnectionID != old.ConnectionID {
					t.Fatal("recovery lost original endpoint, authentication, or ownership")
				}
				if len(forwarded.GetExtraRoute().GetExtraCIDR()) != 1 {
					t.Fatal("recovery lost original connect options")
				}
			}
		})
	}
}

func TestConnectRecovery_StatusErrorIsNotSuccess(t *testing.T) {
	peer := &recoveryPeer{statusErr: status.Error(codes.Unavailable, "temporary root failure")}
	svr, old, req := recoveryUserFixture(t, recoveryGRPCClient(t, peer))
	err := svr.redirectConnectToSudoDaemon(req, &fakeConnectServer{ctx: context.Background()}, logrus.New())
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected root Status failure, got %v", err)
	}
	if len(svr.connections) != 2 || svr.connections[0] != old || old.GetSync() == nil {
		t.Fatal("transient failure discarded metadata")
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.requests) != 0 {
		t.Fatal("transient Status failure triggered rebuilding")
	}
}

func TestConnectRecovery_RebuildFailureCanRetry(t *testing.T) {
	peer := &recoveryPeer{state: StatusFailed, connectErr: status.Error(codes.PermissionDenied, "authentication failed")}
	svr, old, req := recoveryUserFixture(t, recoveryGRPCClient(t, peer))
	err := svr.redirectConnectToSudoDaemon(proto.Clone(req).(*rpc.ConnectRequest), &fakeConnectServer{ctx: context.Background()}, logrus.New())
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("rebuild error hidden: %v", err)
	}
	if len(svr.connections) != 2 || svr.connections[0] != old || old.GetSync() == nil {
		t.Fatal("failed recovery discarded control state")
	}
	peer.mu.Lock()
	peer.connectErr = nil
	peer.mu.Unlock()
	if err := svr.redirectConnectToSudoDaemon(req, &fakeConnectServer{ctx: context.Background()}, logrus.New()); err != nil {
		t.Fatal(err)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.requests) != 2 {
		t.Fatal("explicit retry did not rebuild")
	}
}

func TestConnectRecovery_TerminalStatusOverridesTun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &terminalTestSession{DataSession: &handler.DataSession{ConnectionID: "terminal"}, ctx: ctx}
	// A live session with stale heartbeats remains unhealthy, not terminal.
	if got := resolveStatus(s, nil, true); got != StatusUnhealthy {
		t.Fatalf("live status=%s", got)
	}
	cancel()
	if got := resolveStatus(s, nil, true); got != StatusFailed {
		t.Fatalf("terminal session with lingering TUN reported %s", got)
	}
}

type recoveryDataSession struct {
	*handler.DataSession
	ctx            context.Context
	cleanupStarted chan struct{}
	cleanupRelease chan struct{}
	once           sync.Once
	heartbeat      time.Time
	configMapErr   error
}

func (s *recoveryDataSession) DoConnect(ctx context.Context) error { s.ctx = ctx; return nil }
func (s *recoveryDataSession) Context() context.Context            { return s.ctx }
func (s *recoveryDataSession) GetLastHeartbeat() time.Time         { return s.heartbeat }
func (s *recoveryDataSession) GetLocalTunIP() (string, string)     { return "127.0.0.1", "" }
func (s *recoveryDataSession) GetTrafficManagerConfigMap(context.Context) (*v1.ConfigMap, error) {
	return nil, s.configMapErr
}
func (s *recoveryDataSession) Cleanup(ctx context.Context) {
	s.once.Do(func() {
		if s.cleanupStarted != nil {
			close(s.cleanupStarted)
			<-s.cleanupRelease
		}
		s.DataSession.Cleanup(ctx)
	})
}

func TestConnectRecovery_RootSerializedReplacement(t *testing.T) {
	root := &Server{IsSudo: true, LogFile: &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "root.log")}}
	t.Cleanup(func() { _ = root.LogFile.Close() })
	var built []*recoveryDataSession
	root.newDataSession = func(ds *handler.DataSession) handler.DataPlane {
		s := &recoveryDataSession{DataSession: ds, heartbeat: time.Now()}
		built = append(built, s)
		return s
	}
	cli := recoveryGRPCClient(t, root)
	user, control, req := recoveryUserFixture(t, cli)
	rootReq := proto.Clone(req).(*rpc.ConnectRequest)
	rootReq.ConnectionID = control.ConnectionID
	rootReq.OwnerID = control.OwnerID
	connect := func(req *rpc.ConnectRequest) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := cli.Connect(ctx)
		if err != nil {
			return err
		}
		if err = stream.Send(req); err != nil {
			return err
		}
		_ = stream.CloseSend()
		for {
			_, err = stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	if err := connect(rootReq); err != nil {
		t.Fatal(err)
	}
	otherReq := proto.Clone(rootReq).(*rpc.ConnectRequest)
	otherReq.ConnectionID = "other"
	if err := connect(otherReq); err != nil {
		t.Fatal(err)
	}
	old, other := built[0], built[1]
	old.cleanupStarted = make(chan struct{})
	old.cleanupRelease = make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(old.cleanupRelease) }) })
	// This is the terminal cancellation observed by DataSession after manager failure.
	canceled, cancel := context.WithCancel(context.Background())
	old.ctx = canceled
	old.configMapErr = fmt.Errorf("cache stopped during terminal cleanup")
	cancel()
	cleanupDone := make(chan struct{})
	go func() { old.Cleanup(context.Background()); close(cleanupDone) }()
	<-old.cleanupStarted
	snapshot, err := cli.Status(context.Background(), &rpc.StatusRequest{ConnectionIDs: []string{control.ConnectionID}})
	if err != nil || len(snapshot.GetList()) != 1 || snapshot.List[0].Status != StatusFailed {
		t.Fatalf("terminal root verdict: %v %v", snapshot, err)
	}
	all, err := cli.Status(context.Background(), &rpc.StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range all.List {
		if entry.ConnectionID == control.ConnectionID && entry.Status != StatusFailed {
			t.Fatal("proxy rendering error masked terminal status")
		}
	}
	// User explicit connect and a concurrent direct root replay contend for one slot.
	result := make(chan error, 2)
	go func() {
		result <- user.redirectConnectToSudoDaemon(req, &fakeConnectServer{ctx: context.Background()}, logrus.New())
	}()
	go func() { result <- connect(rootReq) }()
	select {
	case err := <-result:
		t.Fatalf("connect returned before old cleanup joined: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Status must remain responsive while cleanup waits (connMu is not held).
	statusCtx, statusCancel := context.WithTimeout(context.Background(), time.Second)
	_, err = cli.Status(statusCtx, &rpc.StatusRequest{ConnectionIDs: []string{"other"}})
	statusCancel()
	if err != nil {
		t.Fatalf("cleanup blocked registry readers: %v", err)
	}
	release.Do(func() { close(old.cleanupRelease) })
	for i := 0; i < 2; i++ {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reconnect did not complete")
		}
	}
	<-cleanupDone
	root.connMu.Lock()
	replacement, _ := root.findConnection(control.ConnectionID)
	// Simulate a delayed registry retirement using the originally observed instance.
	root.removeConnectionInstance(old)
	retained, _ := root.findConnection(control.ConnectionID)
	sibling, _ := root.findConnection("other")
	count := len(root.connections)
	root.connMu.Unlock()
	if len(built) != 3 || replacement == old || retained != replacement || sibling != other || count != 2 {
		t.Fatal("replacement duplicated, erased by delayed retirement, or removed sibling")
	}
	if user.connections[0] != control || control.GetSync() == nil {
		t.Fatal("control metadata lost")
	}
	if err := connect(rootReq); err != nil {
		t.Fatal(err)
	}
	if len(built) != 3 {
		t.Fatal("healthy replay created duplicate")
	}
	built[2].heartbeat = time.Time{}
	if err := connect(rootReq); err != nil {
		t.Fatal(err)
	}
	if len(built) != 3 {
		t.Fatal("live unhealthy session was replaced")
	}
	// No physical TUN is created: the seam replaces only data-plane startup/cleanup;
	// root request parsing, client initialization, status, registry and gRPC are real.
	for _, s := range built {
		s.Cleanup(context.Background())
	}
}

func TestConnectRecovery_StatusTimeoutPreservesControl(t *testing.T) {
	original := config.SudoStatusTimeout
	config.SudoStatusTimeout = 30 * time.Millisecond
	defer func() { config.SudoStatusTimeout = original }()
	svr, old, req := recoveryUserFixture(t, &blockingStatusClient{})
	start := time.Now()
	err := svr.redirectConnectToSudoDaemon(req, &fakeConnectServer{ctx: context.Background()}, logrus.New())
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded or successful stalled Status: %v", err)
	}
	if len(svr.connections) != 2 || svr.connections[0] != old || old.GetSync() == nil {
		t.Fatal("timeout discarded control state")
	}
}
