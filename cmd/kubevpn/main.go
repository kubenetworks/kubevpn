package main

import (
	_ "net/http/pprof"

	ctrl "sigs.k8s.io/controller-runtime"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
)

func main() {
	ctx := ctrl.SetupSignalHandler()
	_ = cmds.NewKubeVPNCommand().ExecuteContext(ctx)
}
