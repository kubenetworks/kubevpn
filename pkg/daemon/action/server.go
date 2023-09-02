package action

import (
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"time"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	t       time.Time
	connect *handler.ConnectOptions
}
