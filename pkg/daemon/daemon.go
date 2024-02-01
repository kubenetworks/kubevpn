package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/action"
	_ "github.com/wencaiwulue/kubevpn/v2/pkg/daemon/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type SvrOption struct {
	ctx    context.Context
	cancel context.CancelFunc

	uptime int64
	svr    *grpc.Server

	IsSudo bool
}

func (o *SvrOption) Start(ctx context.Context) error {
	l := &lumberjack.Logger{
		Filename:   action.GetDaemonLogPath(),
		MaxSize:    100,
		MaxAge:     3,
		MaxBackups: 3,
		LocalTime:  true,
		Compress:   false,
	}
	util.InitLogger(true)
	log.SetOutput(l)
	// every day 00:00:00 rotate log
	if !o.IsSudo {
		go func() {
			for {
				nowTime := time.Now()
				nowTimeStr := nowTime.Format("2006-01-02")
				t2, _ := time.ParseInLocation("2006-01-02", nowTimeStr, time.Local)
				next := t2.AddDate(0, 0, 1)
				after := next.UnixNano() - nowTime.UnixNano() - 1
				<-time.After(time.Duration(after) * time.Nanosecond)
				_ = l.Rotate()
			}
		}()
	}
	o.ctx, o.cancel = context.WithCancel(ctx)
	var lc net.ListenConfig
	lis, err := lc.Listen(o.ctx, "unix", GetSockPath(o.IsSudo))
	if err != nil {
		return err
	}
	defer lis.Close()

	err = os.Chmod(GetSockPath(o.IsSudo), 0666)
	if err != nil {
		return err
	}

	o.svr = grpc.NewServer()
	cleanup, err := admin.Register(o.svr)
	if err != nil {
		log.Errorf("failed to register admin: %v", err)
		return err
	}
	grpc_health_v1.RegisterHealthServer(o.svr, health.NewServer())
	defer cleanup()
	reflection.Register(o.svr)
	// [tun-client] 223.254.0.101 - 127.0.0.1:8422: dial tcp 127.0.0.1:55407: connect: can't assign requested address
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 100
	// startup a http server
	// With downgrading-capable gRPC server, which can also handle HTTP.
	downgradingServer := &http.Server{}
	defer downgradingServer.Close()
	var h2Server http2.Server
	err = http2.ConfigureServer(downgradingServer, &h2Server)
	if err != nil {
		log.Errorf("failed to configure http2 server: %v", err)
		return err
	}
	handler := CreateDowngradingHandler(o.svr, http.HandlerFunc(http.DefaultServeMux.ServeHTTP))
	downgradingServer.Handler = h2c.NewHandler(handler, &h2Server)
	o.uptime = time.Now().Unix()
	cancel := func() {
		_ = downgradingServer.Close()
		o.Stop()
		_ = l.Rotate()
		_ = l.Close()
	}
	// remember to close http server, otherwise daemon will not quit successfully
	rpc.RegisterDaemonServer(o.svr, &action.Server{Cancel: cancel, IsSudo: o.IsSudo, GetClient: GetClient, LogFile: l})
	return downgradingServer.Serve(lis)
	//return o.svr.Serve(lis)
}

func (o *SvrOption) Stop() {
	o.cancel()
	if o.svr != nil {
		//o.svr.GracefulStop()
		o.svr.Stop()
	}
	path := GetSockPath(o.IsSudo)
	_ = os.Remove(path)
}

// CreateDowngradingHandler takes a gRPC server and a plain HTTP handler, and returns an HTTP handler that has the
// capability of handling HTTP requests and gRPC requests that may require downgrading the response to gRPC-Web or gRPC-WebSocket.
//
//	if r.ProtoMajor == 2 && strings.HasPrefix(
//		r.Header.Get("Content-Type"), "application/grpc") {
//		grpcServer.ServeHTTP(w, r)
//	} else {
//		yourMux.ServeHTTP(w, r)
//	}
func CreateDowngradingHandler(grpcServer *grpc.Server, httpHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			httpHandler.ServeHTTP(w, r)
		}
	})
}
