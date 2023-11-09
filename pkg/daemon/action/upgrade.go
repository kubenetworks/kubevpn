package action

import (
	"context"

	goversion "github.com/hashicorp/go-version"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func (svr *Server) Upgrade(ctx context.Context, req *rpc.UpgradeRequest) (*rpc.UpgradeResponse, error) {
	var err error
	var clientVersion, daemonVersion *goversion.Version
	clientVersion, err = goversion.NewVersion(req.ClientVersion)
	if err != nil {
		err = errors.Wrap(err, "Failed to get new version.")
		return nil, err
	}
	daemonVersion, err = goversion.NewVersion(config.Version)
	if err != nil {
		err = errors.Wrap(err, "Failed to get new version.")
		return nil, err
	}
	if clientVersion.GreaterThan(daemonVersion) || (clientVersion.Equal(daemonVersion) && req.ClientCommitId != config.GitCommit) {
		log.Info("daemon version is less than client, needs to upgrade")
		return &rpc.UpgradeResponse{NeedUpgrade: true}, nil
	}
	return &rpc.UpgradeResponse{NeedUpgrade: false}, nil
}
