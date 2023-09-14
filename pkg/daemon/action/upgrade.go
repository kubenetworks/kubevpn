package action

import (
	"os"

	goversion "github.com/hashicorp/go-version"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Upgrade(req *rpc.UpgradeRequest, resp rpc.Daemon_UpgradeServer) error {
	var err error
	var clientVersion, daemonVersion *goversion.Version
	clientVersion, err = goversion.NewVersion(req.ClientVersion)
	if err != nil {
		return err
	}
	daemonVersion, err = goversion.NewVersion(config.Version)
	if err != nil {
		return err
	}
	if clientVersion.GreaterThan(daemonVersion) || (clientVersion.Equal(daemonVersion) && req.ClientCommitId == config.GitCommit) {
		log.Info("Already up to date, don't needs to upgrade")
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	println(executable)
	return nil
}
