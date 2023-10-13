package main

import (
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	_ "net/http/pprof"

	"github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds"
)

func main() {
	_ = cmds.NewKubeVPNCommand().Execute()
}
