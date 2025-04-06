package main

import (
	ctrl "sigs.k8s.io/controller-runtime"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	_ "k8s.io/cloud-provider-gcp/pkg/clientauthplugin/gcp"
	_ "net/http/pprof"

	"github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
)

func main() {
	ctx := ctrl.SetupSignalHandler()
	_ = cmds.NewKubeVPNCommand().ExecuteContext(ctx)
}
