package main

import (
	ctrl "sigs.k8s.io/controller-runtime"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	_ "net/http/pprof"

	"github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds"
)

func main() {
	ctx := ctrl.SetupSignalHandler()
	_ = cmds.NewKubeVPNCommand().ExecuteContext(ctx)
}
