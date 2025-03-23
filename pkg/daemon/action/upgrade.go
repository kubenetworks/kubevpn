package action

import (
	"context"

	goversion "github.com/hashicorp/go-version"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (svr *Server) Upgrade(ctx context.Context, req *rpc.UpgradeRequest) (*rpc.UpgradeResponse, error) {
	var err error
	var clientVersion, daemonVersion *goversion.Version
	clientVersion, err = goversion.NewVersion(req.ClientVersion)
	if err != nil {
		return nil, err
	}
	daemonVersion, err = goversion.NewVersion(config.Version)
	if err != nil {
		return nil, err
	}
	if clientVersion.GreaterThan(daemonVersion) {
		plog.G(context.Background()).Info("Daemon version is less than client, needs to upgrade")
		return &rpc.UpgradeResponse{NeedUpgrade: true}, nil
	}
	return &rpc.UpgradeResponse{NeedUpgrade: false}, nil
}
