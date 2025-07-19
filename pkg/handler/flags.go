package handler

import (
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func AddCommonFlags(cmd *pflag.FlagSet, transferImage *bool, imagePullSecretName *string, engine *config.Engine) {
	cmd.BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.StringVar(imagePullSecretName, "image-pull-secret-name", *imagePullSecretName, "secret name to pull image if registry is private")
	cmd.BoolVar(transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
}
