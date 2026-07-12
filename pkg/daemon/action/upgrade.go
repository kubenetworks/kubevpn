package action

import (
	"context"

	goversion "github.com/hashicorp/go-version"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Upgrade compares the client version against the daemon version and indicates whether the daemon needs upgrading.
func (svr *Server) Upgrade(ctx context.Context, req *rpc.UpgradeRequest) (*rpc.UpgradeResponse, error) {
	// Dev/CI builds carry a non-semver version (git SHA, "latest", "dev"). Treat an
	// unparseable version on either side as "no upgrade" rather than failing the RPC
	// — the daemon version handshake must not error on dev builds.
	clientVersion, err := goversion.NewVersion(req.ClientVersion)
	if err != nil {
		plog.G(ctx).Debugf("Skip upgrade check: unparseable client version %q: %v", req.ClientVersion, err)
		return &rpc.UpgradeResponse{NeedUpgrade: false}, nil
	}
	daemonVersion, err := goversion.NewVersion(config.Version)
	if err != nil {
		plog.G(ctx).Debugf("Skip upgrade check: unparseable daemon version %q: %v", config.Version, err)
		return &rpc.UpgradeResponse{NeedUpgrade: false}, nil
	}
	if clientVersion.GreaterThan(daemonVersion) {
		plog.G(ctx).Info("Daemon version is less than client, needs to upgrade")
		return &rpc.UpgradeResponse{NeedUpgrade: true}, nil
	}
	return &rpc.UpgradeResponse{NeedUpgrade: false}, nil
}
