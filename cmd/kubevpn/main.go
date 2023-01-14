package main

import (
	_ "net/http/pprof"

	"github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds"
)

func main() {
	_ = cmds.NewKubeVPNCommand().Execute()
}
