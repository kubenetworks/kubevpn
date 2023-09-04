package action

import (
	"time"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	Cancel func()

	t       time.Time
	connect *handler.ConnectOptions
}
