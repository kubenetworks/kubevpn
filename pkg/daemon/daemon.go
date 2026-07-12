package daemon

import (
	"context"
	"errors"
	golog "log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"gopkg.in/natefinch/lumberjack.v2"
	glog "gvisor.dev/gvisor/pkg/log"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/action"
	_ "github.com/wencaiwulue/kubevpn/v2/pkg/daemon/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SvrOption holds the runtime state for a daemon server instance, including its gRPC server and lifecycle controls.
type SvrOption struct {
	ctx    context.Context
	cancel context.CancelFunc

	uptime int64
	svr    *action.Server

	IsSudo bool
	ID     string
}

// Start initializes logging, creates the Unix socket listener, and serves the gRPC daemon until the context is cancelled.
func (o *SvrOption) Start(ctx context.Context) error {
	l := initLogging(o.IsSudo)

	sockPath := config.GetSockPath(o.IsSudo)
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

	err = os.Chmod(sockPath, config.FileModeSocket)
	if err != nil {
		return err
	}

	err = writePIDToFile(o.IsSudo)
	if err != nil {
		return err
	}

	unaryPanicInterceptor := grpc.UnaryInterceptor(rpc.UnaryPanicHandler)
	streamPanicInterceptor := grpc.StreamInterceptor(rpc.StreamPanicHandler)
	svr := grpc.NewServer(unaryPanicInterceptor, streamPanicInterceptor)
	adminCleanup, err := admin.Register(svr)
	if err != nil {
		plog.G(ctx).Errorf("Failed to register admin: %v", err)
		return err
	}
	grpc_health_v1.RegisterHealthServer(svr, health.NewServer())
	defer adminCleanup()
	reflection.Register(svr)
	// [tun-client] 198.18.0.101 - 127.0.0.1:8422: dial tcp 127.0.0.1:55407: connect: can't assign requested address
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 100
	// startup a http server
	// With downgrading-capable gRPC server, which can also handle HTTP.
	downgradingServer := &http.Server{}
	defer downgradingServer.Close()
	var h2Server http2.Server
	err = http2.ConfigureServer(downgradingServer, &h2Server)
	if err != nil {
		plog.G(ctx).Errorf("Failed to configure http2: %v", err)
		return err
	}
	handler := createDowngradingHandler(svr, http.HandlerFunc(http.DefaultServeMux.ServeHTTP))
	downgradingServer.Handler = h2c.NewHandler(handler, &h2Server)
	o.uptime = time.Now().Unix()
	// remember to close http server, otherwise daemon will not quit successfully
	cleanup := func() {
		o.svr.Cancel = nil
		_ = o.svr.Quit(nil)
		_ = downgradingServer.Close()
		_ = l.Close()
	}
	o.svr = &action.Server{Cancel: cleanup, IsSudo: o.IsSudo, GetClient: GetClient, LogFile: l, ID: o.ID}
	if !o.IsSudo {
		go o.svr.LoadFromConfig(o.ctx)
	}
	rpc.RegisterDaemonServer(svr, o.svr)
	return downgradingServer.Serve(lis)
}

// Stop cancels the server context and invokes the server's cleanup callback to shut down gracefully.
func (o *SvrOption) Stop() {
	o.cancel()
	if o.svr != nil && o.svr.Cancel != nil {
		o.svr.Cancel()
	}
}

// createDowngradingHandler takes a gRPC server and a plain HTTP handler, and returns an HTTP handler that has the
// capability of handling HTTP requests and gRPC requests that may require downgrading the response to gRPC-Web or gRPC-WebSocket.
//
//	if r.ProtoMajor == 2 && strings.HasPrefix(
//		r.Header.Get("Content-Type"), "application/grpc") {
//		grpcServer.ServeHTTP(w, r)
//	} else {
//		yourMux.ServeHTTP(w, r)
//	}
func createDowngradingHandler(grpcServer *grpc.Server, httpHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			httpHandler.ServeHTTP(w, r)
		}
	})
}

func (o *SvrOption) detectUnixSocksFile(ctx context.Context) {
	var f = func() {
		_, err := os.Stat(config.GetSockPath(o.IsSudo))
		if errors.Is(err, os.ErrNotExist) {
			o.Stop()
			return
		}
		client, conn, err := getClientWithoutCache(ctx, o.IsSudo)
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
	}
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return
		}
		defer watcher.Close()
		err = watcher.Add(config.GetPidPath(o.IsSudo))
		if err != nil {
			return
		}
		for {
			select {
			case <-watcher.Events:
				f()
			case <-ctx.Done():
				return
			case <-watcher.Errors:
				return
			}
		}
	}()

	const pollInterval = 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
			f()
			time.Sleep(pollInterval)
		}
	}
}

func writePIDToFile(isSudo bool) error {
	pidPath := config.GetPidPath(isSudo)
	pid := os.Getpid()
	return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), config.FileModeFile)
}

// initLogging creates a rotating log file and wires all loggers (logrus, klog, gvisor, plog)
// to write to it. It also suppresses Kubernetes client warnings and starts the daily log rotation goroutine.
func initLogging(isSudo bool) *lumberjack.Logger {
	l := &lumberjack.Logger{
		Filename:   config.GetDaemonLogPath(isSudo),
		MaxSize:    100,
		MaxAge:     3,
		MaxBackups: 3,
		LocalTime:  true,
		Compress:   false,
	}

	log.SetOutput(l)
	golog.Default().SetOutput(l)
	klog.SetOutput(l)
	klog.LogToStderr(false)
	plog.L.SetOutput(l)
	plog.L.SetLevel(log.DebugLevel)
	glog.SetTarget(plog.ServerEmitter{Writer: &glog.Writer{Next: l}})
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
	go rotateLog(l)
	return l
}

// rotateLog rotates the log file daily at midnight.
func rotateLog(l *lumberjack.Logger) {
	for {
		nowTime := time.Now()
		nowTimeStr := nowTime.Format("2006-01-02")
		t2, _ := time.ParseInLocation("2006-01-02", nowTimeStr, time.Local)
		next := t2.AddDate(0, 0, 1)
		after := next.UnixNano() - nowTime.UnixNano()
		<-time.After(time.Duration(after) * time.Nanosecond)
		_ = l.Rotate()
	}
}
