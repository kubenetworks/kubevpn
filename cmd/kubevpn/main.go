package main

import (
	"github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds"

	_ "net/http/pprof"
)

func main() {
	_ = cmds.RootCmd.Execute()
}
