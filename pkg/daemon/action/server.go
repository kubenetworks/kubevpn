package action

import (
	"os"
	"path/filepath"
	"time"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	Cancel    func()
	GetClient func(isSudo bool) rpc.DaemonClient
	IsSudo    bool
	LogFile   *os.File

	t       time.Time
	connect *handler.ConnectOptions
	clone   *handler.CloneOptions
}

func GetDaemonLogPath() string {
	return filepath.Join(config.DaemonPath, config.LogFile)
}
