package rpc

import (
	"context"
	"fmt"
	"runtime/debug"

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
			// Log at Error, NOT Panic: logrus.Panic re-panics after logging, and
			// this runs inside the recover defer where nothing catches it — that
			// second panic would crash the whole daemon and defeat this interceptor.
			plog.G(ctx).Errorf("%s", str)
		}
	}()
	return handler(ctx, req)
}

func StreamPanicHandler(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			str := fmt.Sprintf("Panic: `%s` %s", info.FullMethod, string(debug.Stack()))
			err = status.Error(codes.Internal, str)
			// Log at Error, NOT Panic: logrus.Panic re-panics after logging, and
			// this runs inside the recover defer where nothing catches it — that
			// second panic would crash the whole daemon and defeat this interceptor.
			plog.G(ss.Context()).Errorf("%s", str)
		}
	}()
	return handler(srv, ss)
}
