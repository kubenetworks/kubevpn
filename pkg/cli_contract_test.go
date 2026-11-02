package pkg_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// The CLI's private renderers already have tests in cmd/kubevpn/cmds.
// These contracts exercise the actual protobuf transport and the production
// cancellation helper, without importing cmd, starting a daemon, or reading a
// kubeconfig. The service below stands in for privileged/cluster operations,
// not for the gRPC transport. It does not test action.Server's business logic.
func TestCLIContractStatusSerialization(t *testing.T) {
	fixture := &rpc.StatusResponse{
		CurrentConnectionID: "abc123def456",
		List: []*rpc.Status{{
			ConnectionID: "abc123def456", Cluster: "contract-cluster",
			Kubeconfig: "fixture-only", Namespace: "dev", Status: "connected",
			Netif: "tun-contract", IPv4: "198.18.0.2", IPv6: "fd11::2",
			SocksListenAddr: "127.0.0.1:1080", SocksEgress: true,
			ProxyList: []*rpc.Proxy{{
				ConnectionID: "abc123def456", Workload: "deployments.apps/api",
				RuleList: []*rpc.ProxyRule{{
					Headers:      map[string]string{"user": "alice"},
					LocalTunIPv4: "198.18.0.2", LocalTunIPv6: "fd11::2",
					CurrentDevice: true, PortMap: map[int32]int32{8080: 18080},
				}},
			}},
			SyncList: []*rpc.Sync{{
				ConnectionID: "abc123def456", Workload: "deployments.apps/worker",
				SyncthingGUIAddr: "127.0.0.1:8384",
				RuleList:         []*rpc.SyncRule{{Headers: map[string]string{"user": "alice"}, DstWorkload: "deployments.apps/worker-sync"}},
			}},
		}},
	}
	server := &cliContractServer{
		status:         fixture,
		statusRequests: make(chan *rpc.StatusRequest, 2),
	}
	client := newCLIContractClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := &rpc.StatusRequest{ConnectionIDs: []string{"abc123def456"}}
	got, err := client.Status(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-server.statusRequests:
		if !proto.Equal(received, request) {
			t.Fatalf("Status filter changed in transit: %v", received)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !proto.Equal(got, fixture) {
		t.Fatalf("Status protobuf round trip changed fields: %v", got)
	}

	// status -o json uses encoding/json, NOT protojson. Check independently
	// written JSON keys/values so changing generated JSON tags cannot silently
	// change the CLI contract while a same-type round trip still passes.
	wantJSON := `{
		"CurrentConnectionID":"abc123def456",
		"List":[{
			"ConnectionID":"abc123def456","Cluster":"contract-cluster",
			"Kubeconfig":"fixture-only","Namespace":"dev","Status":"connected",
			"Netif":"tun-contract","IPv4":"198.18.0.2","IPv6":"fd11::2",
			"SocksListenAddr":"127.0.0.1:1080","SocksEgress":true,
			"ProxyList":[{"ConnectionID":"abc123def456","Workload":"deployments.apps/api",
				"RuleList":[{"Headers":{"user":"alice"},"LocalTunIPv4":"198.18.0.2",
					"LocalTunIPv6":"fd11::2","CurrentDevice":true,"PortMap":{"8080":18080}}]}],
			"SyncList":[{"ConnectionID":"abc123def456","Workload":"deployments.apps/worker",
				"SyncthingGUIAddr":"127.0.0.1:8384",
				"RuleList":[{"Headers":{"user":"alice"},"DstWorkload":"deployments.apps/worker-sync"}]}]
		}]
	}`
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actual, expected any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("CLI JSON contract changed:\n%s", encoded)
	}

	// The fixture returns an empty response for this request. This checks only
	// empty-response transport/JSON semantics, not production filter behavior.
	empty, err := client.Status(ctx, &rpc.StatusRequest{ConnectionIDs: []string{"missing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.List) != 0 || empty.CurrentConnectionID != "" {
		t.Fatalf("empty fixture response changed in transit: %v", empty)
	}
	encoded, err = json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("empty StatusResponse must retain omitempty semantics: %s", encoded)
	}
}

func TestCLIContractConnectProgressAndCancel(t *testing.T) {
	for _, explicitCancel := range []bool{false, true} {
		name := "half_close_keeps_session"
		if explicitCancel {
			name = "explicit_cancel_ends_session"
		}
		t.Run(name, func(t *testing.T) {
			server := &cliContractServer{connectResults: make(chan cliContractConnectResult, 1)}
			client := newCLIContractClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			request := &rpc.ConnectRequest{
				// Deliberately not a usable kubeconfig: the fake resource never
				// initializes Kubernetes, SSH, TUN, or daemon persistence.
				KubeconfigBytes: "fixture-only", Namespace: "dev", ManagerNamespace: "manager",
				OwnerID: "owner-alice", ConnectionID: "abc123def456", Level: 4,
				EnableSocks: true, SocksListen: "127.0.0.1:1080", SocksEgress: true,
				ExtraRoute: &rpc.ExtraRoute{ExtraCIDR: []string{"10.0.0.0/24"}, ExtraDomain: []string{"api.invalid"}},
			}
			if err := stream.Send(request); err != nil {
				t.Fatal(err)
			}
			// Literal sentinels pin the frozen CLI wire format independently
			// of EncodeStep; rendering itself is covered by cmd's tests.
			want := []*rpc.ConnectResponse{
				{Message: "\x1fSForwarding ports\n"},
				{Message: "\x1fDForwarded ports\n"},
				{ConnectionID: "abc123def456"},
			}
			for i, expected := range want {
				got, err := stream.Recv()
				if err != nil {
					t.Fatalf("progress frame %d: %v", i, err)
				}
				if !proto.Equal(got, expected) {
					t.Fatalf("progress frame %d: got %v, want %v", i, got, expected)
				}
			}
			if explicitCancel {
				// This is precisely the CLI protocol: Cancel is sent via the
				// untyped stream after the initial typed ConnectRequest.
				if err := stream.SendMsg(&rpc.Cancel{}); err != nil {
					t.Fatal(err)
				}
			} else if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Recv()
			if explicitCancel {
				if status.Code(err) != codes.Canceled {
					t.Fatalf("explicit Cancel: got %v, want Canceled", err)
				}
			} else if !errors.Is(err, io.EOF) {
				t.Fatalf("normal completion: got %v, want EOF", err)
			}
			select {
			case result := <-server.connectResults:
				if !proto.Equal(result.request, request) {
					t.Fatalf("Connect request changed in transit: %v", result.request)
				}
				if result.canceled != explicitCancel {
					t.Fatalf("session canceled = %v, want %v", result.canceled, explicitCancel)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

type cliContractConnectResult struct {
	request  *rpc.ConnectRequest
	canceled bool
}

type cliContractServer struct {
	rpc.UnimplementedDaemonServer
	status         *rpc.StatusResponse
	statusRequests chan *rpc.StatusRequest
	connectResults chan cliContractConnectResult
}

func (s *cliContractServer) Status(_ context.Context, request *rpc.StatusRequest) (*rpc.StatusResponse, error) {
	s.statusRequests <- request
	for _, id := range request.ConnectionIDs {
		if id == s.status.CurrentConnectionID {
			return s.status, nil
		}
	}
	return &rpc.StatusResponse{}, nil
}

func (s *cliContractServer) Connect(stream rpc.Daemon_ConnectServer) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	for _, response := range []*rpc.ConnectResponse{
		{Message: plog.EncodeStep(plog.StepBegin, "Forwarding ports\n")},
		{Message: plog.EncodeStep(plog.StepEnd, "Forwarded ports\n")},
		{ConnectionID: request.ConnectionID},
	} {
		if err := stream.Send(response); err != nil {
			return err
		}
	}
	// Check that ListenCancel leaves the session alive after a normal half-close.
	// It runs synchronously here, so the assertion requires neither sleeps nor
	// a race-prone check of a background goroutine's progress.
	session, cancel := context.WithCancel(context.Background())
	defer cancel()
	grpcutil.ListenCancel(stream, cancel)
	canceled := session.Err() != nil
	s.connectResults <- cliContractConnectResult{request: request, canceled: canceled}
	if canceled {
		return status.Error(codes.Canceled, "connect canceled")
	}
	return nil
}

func newCLIContractClient(t *testing.T, service rpc.DaemonServer) rpc.DaemonClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	rpc.RegisterDaemonServer(server, service)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("bufconn server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("bufconn server did not stop")
		}
	})
	conn, err := grpc.NewClient("passthrough:///cli-contract",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return rpc.NewDaemonClient(conn)
}
