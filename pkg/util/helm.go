package util

import (
	"context"
    "fmt"
	"os"

	"github.com/pkg/errors"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/release/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetHelmInstalledNamespace
//  1. use helm to install kubevpn server, means cluster mode,
//     all kubevpn client should connect to this namespace.
//  2. if any error occurs, just ignore and will use options `-n` or `--namespace`
func GetHelmInstalledNamespace(ctx context.Context, f cmdutil.Factory) (string, error) {
	cfg := new(action.Configuration)
	client := action.NewList(cfg)
	var nothing = func(format string, v ...interface{}) {}
	err := cfg.Init(f, "", os.Getenv("HELM_DRIVER"), nothing)
	if err != nil {
		return "", err
	}
	client.SetStateMask()
	releases, err := client.Run()
	if err != nil {
		return "", err
	}
	for _, app := range releases {
		if app.Name == config.HelmAppNameKubevpn &&
			app.Info != nil && app.Info.Status == v1.StatusDeployed {
			return app.Namespace, nil
		}
	}
	return "", errors.New(fmt.Sprintf("app %s not found", config.HelmAppNameKubevpn))
}
