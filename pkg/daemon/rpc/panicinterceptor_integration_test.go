package rpc

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// panicinterceptor_integration_test.go wires a real gRPC server with the daemon's
// panic-recovery interceptors to a real client and asserts the discriminating
// property: a handler that panics must surface as codes.Internal to the caller
// AND must NOT bring down the daemon — a subsequent RPC on the same server still
// succeeds. Before the fix the interceptor called logrus.Panic (which re-panics
// after logging) inside the recover defer, crashing the whole process; that
// crash would take the test binary down instead of leaving a clean assertion.

// TestUnaryPanicHandler_RecoversWithoutCrashing exercises the unary interceptor.
func TestUnaryPanicHandler_RecoversWithoutCrashing(t *testing.T) {
	var calls int
	// First call panics; second call returns normally. If the panic interceptor
	// re-panics, the process dies before the second call can succeed.
	impl := &classifyTestServer{forward: func() error {
		calls++
		if calls == 1 {
			panic("boom unary")
		}
		return nil
	}}

	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(UnaryPanicHandler))
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

	// Panicking call: the recovered panic must surface as codes.Internal.
	err = conn.Invoke(context.Background(), classifyTestMethod, &emptypb.Empty{}, &emptypb.Empty{})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("panicking call: status code = %v, want Internal (err=%v)", got, err)
	}

	// Daemon survived: a follow-up call on the same server still works.
	if err = conn.Invoke(context.Background(), classifyTestMethod, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatalf("follow-up call after panic failed (daemon crashed?): %v", err)
	}
}

const panicStreamMethod = "/test.PanicStreamSvc/Do"

type panicStreamServer struct {
	calls int
}

// panicStreamDesc is an ad-hoc streaming service so the test needs no generated proto.
var panicStreamDesc = grpc.ServiceDesc{
	ServiceName: "test.PanicStreamSvc",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{
		{
			StreamName: "Do",
			Handler: func(srv any, stream grpc.ServerStream) error {
				s := srv.(*panicStreamServer)
				s.calls++
				if s.calls == 1 {
					panic("boom stream")
				}
				return nil
			},
			ServerStreams: true,
			ClientStreams: true,
		},
	},
}

// TestStreamPanicHandler_RecoversWithoutCrashing exercises the stream interceptor.
func TestStreamPanicHandler_RecoversWithoutCrashing(t *testing.T) {
	srv := grpc.NewServer(grpc.ChainStreamInterceptor(StreamPanicHandler))
	srv.RegisterService(&panicStreamDesc, &panicStreamServer{})
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

	streamDesc := &grpc.StreamDesc{StreamName: "Do", ServerStreams: true, ClientStreams: true}

	// Panicking stream: recovered panic must surface as codes.Internal.
	st, err := conn.NewStream(context.Background(), streamDesc, panicStreamMethod)
	if err != nil {
		t.Fatalf("new stream: %v", err)
	}
	_ = st.CloseSend()
	err = st.RecvMsg(&emptypb.Empty{})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("panicking stream: status code = %v, want Internal (err=%v)", got, err)
	}

	// Daemon survived: a follow-up stream on the same server completes cleanly.
	st2, err := conn.NewStream(context.Background(), streamDesc, panicStreamMethod)
	if err != nil {
		t.Fatalf("new stream (2): %v", err)
	}
	_ = st2.CloseSend()
	if err = st2.RecvMsg(&emptypb.Empty{}); err != nil && err != io.EOF {
		t.Fatalf("follow-up stream after panic failed (daemon crashed?): %v", err)
	}
}
