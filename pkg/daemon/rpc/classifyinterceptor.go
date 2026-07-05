package rpc

import (
	stderrors "errors"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

var _ grpc.UnaryServerInterceptor = UnaryClassifyInterceptor
var _ grpc.StreamServerInterceptor = StreamClassifyInterceptor

// UnaryClassifyInterceptor tags handler errors with a gRPC status code so the CLI
// can derive a process exit code. It runs inside the panic interceptor: a panic is
// already codes.Internal (non-Unknown) and is passed through untouched. An error
// that already carries a non-Unknown code — e.g. one forwarded from the root daemon
// over the user→root hop — is also preserved, so classification survives both hops.
func UnaryClassifyInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	return resp, classifyError(err)
}

// StreamClassifyInterceptor is the streaming counterpart of UnaryClassifyInterceptor.
func StreamClassifyInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return classifyError(handler(srv, ss))
}

// classifyError attaches a gRPC status code to err based on known sentinel errors.
// It is a no-op for nil and for errors that already carry a non-Unknown gRPC code
// (preserving codes set by a panic interceptor or forwarded from another daemon).
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return err
	}
	switch {
	case stderrors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case stderrors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case stderrors.Is(err, config.ErrInvalidKubeconfig):
		return status.Error(codes.InvalidArgument, err.Error())
	case stderrors.Is(err, config.ErrPortForwardTimeout):
		return status.Error(codes.Unavailable, err.Error())
	case stderrors.Is(err, config.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case stderrors.Is(err, config.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Unknown, err.Error())
	}
}
