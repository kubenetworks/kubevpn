package rpc

import (
	"context"
	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util/exitcode"
)

var _ grpc.UnaryServerInterceptor = UnaryClassifyInterceptor
var _ grpc.StreamServerInterceptor = StreamClassifyInterceptor

// UnaryClassifyInterceptor tags handler errors with a gRPC status carrying a rich
// kubevpn exit code (see pkg/util/exitcode). It runs inside the panic interceptor:
// a panic is already codes.Internal and is passed through untouched, as is any error
// already classified — including one forwarded from the root daemon — so a code is
// set once and survives the user→root daemon hop.
func UnaryClassifyInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	return resp, exitcode.AsStatusError(err)
}

// StreamClassifyInterceptor is the streaming counterpart of UnaryClassifyInterceptor.
func StreamClassifyInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return exitcode.AsStatusError(handler(srv, ss))
}
