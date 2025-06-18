package action

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
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

	connect          *handler.ConnectOptions
	clone            *handler.CloneOptions
	secondaryConnect []*handler.ConnectOptions

	ID string
}

type Config struct {
	Connect          *handler.ConnectOptions   `json:"Connect"`
	SecondaryConnect []*handler.ConnectOptions `json:"SecondaryConnect"`
}

func (svr *Server) LoadFromConfig() error {
	content, err := os.ReadFile(config.GetDBPath())
	if err != nil {
		return err
	}
	jsonConf, err := yaml.YAMLToJSON(content)
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
	if conf.Connect != nil {
		var resp rpc.Daemon_ConnectClient
		resp, err = client.Connect(context.Background())
		if err == nil {
			conf.Connect.Request.IPv4 = conf.Connect.LocalTunIPv4.String()
			conf.Connect.Request.IPv6 = conf.Connect.LocalTunIPv6.String()
			err = resp.Send(conf.Connect.Request)
			_ = util.PrintGRPCStream[rpc.ConnectResponse](nil, resp, svr.LogFile)
		}
	}
	for _, c := range conf.SecondaryConnect {
		if c != nil {
			var resp rpc.Daemon_ConnectClient
			resp, err = client.ConnectFork(context.Background())
			if err != nil {
				continue
			}
			c.Request.IPv4 = c.LocalTunIPv4.String()
			c.Request.IPv6 = c.LocalTunIPv6.String()
			err = resp.Send(c.Request)
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
