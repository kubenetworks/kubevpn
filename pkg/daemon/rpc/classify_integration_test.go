package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/exitcode"
)

// classify_integration_test.go wires a real gRPC server (with the same interceptor
// chain as the daemon) to a real client and verifies that handler errors surface as
// the expected gRPC status code and process exit code, including across a simulated
// user-daemon → root-daemon hop.

const classifyTestMethod = "/test.ClassifySvc/Do"

type classifyTestServer struct {
	err     error
	forward func() error // when set, the handler calls this (simulating a downstream daemon)
}

func (s *classifyTestServer) do() (*emptypb.Empty, error) {
	if s.forward != nil {
		return &emptypb.Empty{}, s.forward()
	}
	return &emptypb.Empty{}, s.err
}

// classifyTestDesc is an ad-hoc unary service so the test needs no generated proto.
var classifyTestDesc = grpc.ServiceDesc{
	ServiceName: "test.ClassifySvc",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Do",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				handler := func(ctx context.Context, req any) (any, error) {
					return srv.(*classifyTestServer).do()
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: classifyTestMethod}
				if interceptor == nil {
					return handler(ctx, in)
				}
				return interceptor(ctx, in, info, handler)
			},
		},
	},
}

// startClassifyServer starts a gRPC server with the daemon's interceptor chain and
// returns a connected client.
func startClassifyServer(t *testing.T, impl *classifyTestServer) *grpc.ClientConn {
	t.Helper()
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryPanicHandler, UnaryClassifyInterceptor),
	)
	srv.RegisterService(&classifyTestDesc, impl)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis)
	conn, err := grpc.DialContext(context.Background(), lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		srv.Stop()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.Stop()
	})
	return conn
}

func callClassify(conn *grpc.ClientConn) error {
	return conn.Invoke(context.Background(), classifyTestMethod, &emptypb.Empty{}, &emptypb.Empty{})
}

func TestClassifyInterceptor_SentinelMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
		wantExit int
	}{
		{"invalid kubeconfig", fmt.Errorf("load: %w", config.ErrInvalidKubeconfig), codes.InvalidArgument, exitcode.BadConfig},
		{"port-forward timeout", fmt.Errorf("pf: %w", config.ErrPortForwardTimeout), codes.Unavailable, exitcode.ClusterNetwork},
		{"permission", fmt.Errorf("rbac: %w", config.ErrPermissionDenied), codes.PermissionDenied, exitcode.Permission},
		{"not found", fmt.Errorf("get: %w", config.ErrNotFound), codes.NotFound, exitcode.NotFound},
		{"context canceled", context.Canceled, codes.Canceled, exitcode.Interrupted},
		{"plain error", errors.New("boom"), codes.Unknown, exitcode.Generic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := startClassifyServer(t, &classifyTestServer{err: tt.err})
			err := callClassify(conn)
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("status code = %v, want %v (err=%v)", got, tt.wantCode, err)
			}
			if got := exitcode.FromError(err); got != tt.wantExit {
				t.Fatalf("exit code = %d, want %d (err=%v)", got, tt.wantExit, err)
			}
		})
	}
}

// TestClassifyInterceptor_PreservesPreCodedError verifies the classify interceptor
// leaves an already-coded error untouched. This is the contract the panic interceptor
// relies on: it sets codes.Internal, and classify (running inside it) must not clobber
// that code. A handler returning a pre-coded codes.Internal stands in for that case.
func TestClassifyInterceptor_PreservesPreCodedError(t *testing.T) {
	conn := startClassifyServer(t, &classifyTestServer{err: status.Error(codes.Internal, "boom")})
	err := callClassify(conn)
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("status code = %v, want Internal", got)
	}
	if got := exitcode.FromError(err); got != exitcode.Generic {
		t.Fatalf("exit code = %d, want %d", got, exitcode.Generic)
	}
}

// TestClassifyInterceptor_TwoHopPreservesCode simulates the user-daemon → root-daemon
// hop: server B classifies a wrapped ErrNotFound into codes.NotFound; server A forwards
// B's error verbatim and its classify interceptor must preserve the code, so the outer
// client still sees NotFound.
func TestClassifyInterceptor_TwoHopPreservesCode(t *testing.T) {
	connB := startClassifyServer(t, &classifyTestServer{err: fmt.Errorf("root daemon: %w", config.ErrNotFound)})
	connA := startClassifyServer(t, &classifyTestServer{forward: func() error { return callClassify(connB) }})

	err := callClassify(connA)
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("two-hop status code = %v, want NotFound", got)
	}
	if got := exitcode.FromError(err); got != exitcode.NotFound {
		t.Fatalf("two-hop exit code = %d, want %d", got, exitcode.NotFound)
	}
}
