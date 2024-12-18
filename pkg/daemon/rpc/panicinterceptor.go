package rpc

import (
	"fmt"
	"runtime/debug"

	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ grpc.UnaryServerInterceptor = UnaryPanicHandler
var _ grpc.StreamServerInterceptor = StreamPanicHandler

func UnaryPanicHandler(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			str := fmt.Sprintf("Panic: `%s` %s", info.FullMethod, string(debug.Stack()))
			err = status.Error(codes.Internal, str)
			logrus.Panic(str)
		}
	}()
	return handler(ctx, req)
}

func StreamPanicHandler(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			str := fmt.Sprintf("Panic: `%s` %s", info.FullMethod, string(debug.Stack()))
			err = status.Error(codes.Internal, str)
			logrus.Panic(str)
		}
	}()
	return handler(srv, ss)
}
