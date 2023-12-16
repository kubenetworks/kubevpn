package action

import (
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/restmapper"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	Cancel    func()
	GetClient func(isSudo bool) rpc.DaemonClient
	IsSudo    bool
	LogFile   *lumberjack.Logger
	Lock      sync.Mutex

	t                time.Time
	connect          *handler.ConnectOptions
	clone            *handler.CloneOptions
	secondaryConnect []*handler.ConnectOptions

	gr       []*restmapper.APIGroupResources
	informer metadatainformer.SharedInformerFactory
}

func GetDaemonLogPath() string {
	return filepath.Join(config.DaemonPath, config.LogFile)
}
