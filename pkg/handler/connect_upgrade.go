package handler

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cluster/manager"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (c *ConnectOptions) UpgradeDeploy(ctx context.Context) error {
	serverImg, runningPodImg, err := manager.Images(ctx, c.clientset, c.ManagerNamespace)
	if err != nil {
		return err
	}
	// Keep the CLI compatibility policy (including its error banner) at the
	// session boundary, and check the running pod as well during rollback.
	isNeedUpgrade, err := util.IsNewer(config.Version, c.Image, serverImg)
	if err != nil {
		return err
	}
	isPodNeedUpgrade, err := util.IsNewer(config.Version, c.Image, runningPodImg)
	if err != nil {
		return err
	}
	if !isNeedUpgrade && !isPodNeedUpgrade {
		return nil
	}
	plog.StepStart(ctx, "Upgrading traffic manager")
	plog.G(ctx).Debugf("Set image %s --> %s...", serverImg, c.Image)
	if err := manager.Upgrade(ctx, c.factory, c.ManagerNamespace, c.Image); err != nil {
		return err
	}
	plog.StepDone(ctx, "Upgraded traffic manager to %s", config.Version)
	return nil
}
