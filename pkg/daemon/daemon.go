package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/admin"
	"google.golang.org/grpc/reflection"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/action"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type SvrOption struct {
	ctx    context.Context
	cancel context.CancelFunc

	uptime int64
	svr    *grpc.Server

	IsSudo bool
	Port   int
}

func (o *SvrOption) Start(ctx context.Context) error {
	o.ctx, o.cancel = context.WithCancel(ctx)
	var lc net.ListenConfig
	lis, err := lc.Listen(o.ctx, "tcp", fmt.Sprintf(":%d", o.Port))
	if err != nil {
		return err
	}
	defer lis.Close()

	portPath := GetPortPath(o.IsSudo)
	err = os.WriteFile(portPath, []byte(strconv.Itoa(lis.Addr().(*net.TCPAddr).Port)), os.ModePerm)
	if err != nil {
		return err
	}
	err = os.Chmod(portPath, os.ModePerm)
	if err != nil {
		return err
	}

	o.svr = grpc.NewServer()
	cleanup, err := admin.Register(o.svr)
	if err != nil {
		log.Errorf("failed to register admin: %v", err)
		return err
	}
	defer cleanup()
	reflection.Register(o.svr)
	rpc.RegisterDaemonServer(o.svr, &action.Server{Cancel: o.Stop})
	o.uptime = time.Now().Unix()
	return o.svr.Serve(lis)
}

func (o *SvrOption) Stop() {
	o.cancel()
	if o.svr != nil {
		//o.svr.GracefulStop()
		o.svr.Stop()
	}
}
