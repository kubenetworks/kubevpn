package main

import (
	"github.com/wencaiwulue/kubevpn/cmd"
	"github.com/wencaiwulue/kubevpn/util"
)

func main() {
	if !util.IsAdmin() {
		util.RunWithElevated()
	} else {
		_ = cmd.RootCmd.Execute()
	}
}
