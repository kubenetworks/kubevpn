package rpc

import (
	"fmt"
	"runtime/debug"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

var _ grpc.UnaryServerInterceptor = UnaryPanicHandler
var _ grpc.StreamServerInterceptor = StreamPanicHandler

func UnaryPanicHandler(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			str := fmt.Sprintf("Panic: `%s` %s", info.FullMethod, string(debug.Stack()))
			err = status.Error(codes.Internal, str)
			plog.G(context.Background()).Panic(str)
		}
	}()
	return handler(ctx, req)
}

func StreamPanicHandler(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			str := fmt.Sprintf("Panic: `%s` %s", info.FullMethod, string(debug.Stack()))
			err = status.Error(codes.Internal, str)
			plog.G(context.Background()).Panic(str)
		}
	}()
	return handler(srv, ss)
}
