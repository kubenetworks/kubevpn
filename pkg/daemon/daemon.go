package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/action"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type SvrOption struct {
	ctx    context.Context
	cancel context.CancelFunc

	uptime int64
	svr    *grpc.Server

	IsSudo bool
}

func (o *SvrOption) Start(ctx context.Context) error {
	file, err := os.OpenFile(action.GetDaemonLogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		log.Error(err)
		return err
	}
	util.InitLogger(true)
	log.StandardLogger().SetOutput(file)

	o.ctx, o.cancel = context.WithCancel(ctx)
	var lc net.ListenConfig
	lis, err := lc.Listen(o.ctx, "unix", GetSockPath(o.IsSudo))
	if err != nil {
		return err
	}
	defer lis.Close()

	err = os.Chmod(GetSockPath(o.IsSudo), os.ModePerm)
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
	rpc.RegisterDaemonServer(o.svr, &action.Server{Cancel: o.Stop, IsSudo: o.IsSudo, GetClient: GetClient})
	o.uptime = time.Now().Unix()
	return o.svr.Serve(lis)
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
