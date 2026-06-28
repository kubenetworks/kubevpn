package handler

import (
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func AddCommonFlags(cmd *pflag.FlagSet, transferImage *bool, imagePullSecretName *string) {
	AddDebugFlag(cmd)
	cmd.StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.StringVar(imagePullSecretName, "image-pull-secret-name", *imagePullSecretName, "secret name to pull image if registry is private")
	cmd.BoolVar(transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
}

// AddDebugFlag binds the --debug flag to config.Debug. It is the single place commands opt into
// debug logging: --debug makes the CLI display Debug-level logs (including the daemon's, streamed
// at this level) and is carried to the daemon via each request's Level field. Used standalone by
// commands that don't take the full common flag set (leave, reset, disconnect, unsync, uninstall,
// quit), and via AddCommonFlags by connect/proxy/sync/run.
func AddDebugFlag(cmd *pflag.FlagSet) {
	cmd.BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
}
