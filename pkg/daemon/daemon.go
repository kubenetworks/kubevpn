package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
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
	svr    *action.Server

	IsSudo bool
	ID     string
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

	sockPath := GetSockPath(o.IsSudo)
	err := os.Remove(sockPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	o.ctx, o.cancel = context.WithCancel(ctx)
	var lc net.ListenConfig
	lis, err := lc.Listen(o.ctx, "unix", sockPath)
	if err != nil {
		return err
	}
	defer lis.Close()
	lis.(*net.UnixListener).SetUnlinkOnClose(false)
	go o.detectUnixSocksFile(o.ctx)

	err = os.Chmod(sockPath, 0666)
	if err != nil {
		return err
	}

	err = writePIDToFile(o.IsSudo)
	if err != nil {
		return err
	}

	svr := grpc.NewServer()
	cleanup, err := admin.Register(svr)
	if err != nil {
		log.Errorf("failed to register admin: %v", err)
		return err
	}
	grpc_health_v1.RegisterHealthServer(svr, health.NewServer())
	defer cleanup()
	reflection.Register(svr)
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
	handler := CreateDowngradingHandler(svr, http.HandlerFunc(http.DefaultServeMux.ServeHTTP))
	downgradingServer.Handler = h2c.NewHandler(handler, &h2Server)
	o.uptime = time.Now().Unix()
	// remember to close http server, otherwise daemon will not quit successfully
	cancel := func() {
		o.svr.Cancel = nil
		_ = o.svr.Quit(&rpc.QuitRequest{}, nil)
		_ = downgradingServer.Close()
		_ = l.Rotate()
		_ = l.Close()
	}
	o.svr = &action.Server{Cancel: cancel, IsSudo: o.IsSudo, GetClient: GetClient, LogFile: l, ID: o.ID}
	rpc.RegisterDaemonServer(svr, o.svr)
	return downgradingServer.Serve(lis)
	//return o.svr.Serve(lis)
}

func (o *SvrOption) Stop() {
	o.cancel()
	if o.svr != nil && o.svr.Cancel != nil {
		//o.svr.GracefulStop()
		o.svr.Cancel()
	}
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

func (o *SvrOption) detectUnixSocksFile(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				_, err := os.Stat(GetSockPath(o.IsSudo))
				if errors.Is(err, os.ErrNotExist) {
					o.Stop()
					return
				}
				client, conn, err := GetClientWithoutCache(ctx, o.IsSudo)
				if conn != nil {
					defer conn.Close()
				}
				if err != nil {
					return
				}

				var identify *rpc.IdentifyResponse
				identify, err = client.Identify(ctx, &rpc.IdentifyRequest{})
				if err != nil {
					return
				}
				if identify.ID != o.ID {
					o.Stop()
					return
				}
			}()
		}
	}
}

func writePIDToFile(isSudo bool) error {
	pidPath := GetPidPath(isSudo)
	pid := os.Getpid()
	err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), os.ModePerm)
	if err != nil {
		return err
	}
	err = os.Chmod(pidPath, os.ModePerm)
	return err
}
