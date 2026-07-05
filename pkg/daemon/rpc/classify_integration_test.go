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

// TestClassifyInterceptor_RichCodeOverWire verifies the rich exit code (carried in the
// gRPC status detail, not the coarse code) travels from the daemon to the CLI.
func TestClassifyInterceptor_RichCodeOverWire(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantExit int
	}{
		{"kubeconfig invalid", fmt.Errorf("load: %w", config.ErrInvalidKubeconfig), exitcode.KubeconfigInvalid},
		{"port-forward timeout", fmt.Errorf("pf: %w", config.ErrPortForwardTimeout), exitcode.PortForwardTimeout},
		{"dhcp exhausted", fmt.Errorf("dhcp: %w", config.ErrDHCPExhausted), exitcode.DHCPExhausted},
		{"tun device", fmt.Errorf("tun: %w", config.ErrTunDeviceFailed), exitcode.TunDeviceFailed},
		{"connection not found", fmt.Errorf("x: %w", config.ErrConnectionNotFound), exitcode.ConnectionNotFound},
		{"context canceled", context.Canceled, exitcode.Interrupted},
		{"plain error", errors.New("boom"), exitcode.Generic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := startClassifyServer(t, &classifyTestServer{err: tt.err})
			err := callClassify(conn)
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
	if got := exitcode.FromError(err); got != exitcode.Internal {
		t.Fatalf("exit code = %d, want %d", got, exitcode.Internal)
	}
}

// TestClassifyInterceptor_TwoHopPreservesRichCode simulates the user-daemon → root-daemon
// hop: server B classifies a wrapped ErrDHCPExhausted into a rich-coded status; server A
// forwards B's error verbatim and its classify interceptor must preserve the detail, so
// the outer client still derives the precise DHCPExhausted exit code.
func TestClassifyInterceptor_TwoHopPreservesRichCode(t *testing.T) {
	connB := startClassifyServer(t, &classifyTestServer{err: fmt.Errorf("root daemon: %w", config.ErrDHCPExhausted)})
	connA := startClassifyServer(t, &classifyTestServer{forward: func() error { return callClassify(connB) }})

	err := callClassify(connA)
	if got := exitcode.FromError(err); got != exitcode.DHCPExhausted {
		t.Fatalf("two-hop exit code = %d, want %d", got, exitcode.DHCPExhausted)
	}
}
