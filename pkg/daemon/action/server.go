package action

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/metadata/metadatainformer"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Server struct {
	rpc.UnimplementedDaemonServer

	Cancel    func()
	GetClient func(isSudo bool) (rpc.DaemonClient, error)
	IsSudo    bool
	LogFile   *lumberjack.Logger
	Lock      sync.Mutex

	t                time.Time
	connect          *handler.ConnectOptions
	clone            *handler.CloneOptions
	secondaryConnect []*handler.ConnectOptions

	resourceLists []*metav1.APIResourceList
	informer      metadatainformer.SharedInformerFactory

	ID string
}

type Config struct {
	Connect          *handler.ConnectOptions   `json:"Connect"`
	SecondaryConnect []*handler.ConnectOptions `json:"SecondaryConnect"`
}

func (svr *Server) LoadFromConfig() error {
	file, err := os.ReadFile(config.GetDBPath())
	if err != nil {
		return err
	}
	jsonConf, err := yaml.YAMLToJSON(file)
	if err != nil {
		return err
	}
	var conf Config
	err = json.Unmarshal(jsonConf, &conf)
	if err != nil {
		return err
	}
	if conf.Connect == nil && len(conf.SecondaryConnect) == 0 {
		return nil
	}
	var client rpc.DaemonClient
	for {
		_, err = svr.GetClient(true)
		if err != nil {
			time.Sleep(time.Millisecond * 200)
			continue
		}
		client, err = svr.GetClient(false)
		if err != nil {
			time.Sleep(time.Millisecond * 200)
			continue
		}
		break
	}
	for _, conf := range append(conf.SecondaryConnect, conf.Connect) {
		if conf != nil {
			var resp rpc.Daemon_ConnectClient
			resp, err = client.Connect(context.Background())
			if err != nil {
				continue
			}
			conf.Request.IPv4 = conf.LocalTunIPv4.String()
			conf.Request.IPv6 = conf.LocalTunIPv6.String()
			err = resp.Send(conf.Request)
			_ = util.PrintGRPCStream[rpc.ConnectResponse](nil, resp, svr.LogFile)
		}
	}
	return nil
}

func (svr *Server) OffloadToConfig() error {
	conf := &Config{
		Connect:          svr.connect,
		SecondaryConnect: svr.secondaryConnect,
	}
	jsonConf, err := json.Marshal(conf)
	if err != nil {
		return err
	}
	yamlConf, err := yaml.JSONToYAML(jsonConf)
	if err != nil {
		return err
	}
	err = os.WriteFile(config.GetDBPath(), yamlConf, 0644)
	return err
}

func (svr *Server) CleanupConfig() error {
	return os.Remove(config.GetDBPath())
}
