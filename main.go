package main

import (
	"github.com/wencaiwulue/kubevpn/pkg"
	"github.com/wencaiwulue/kubevpn/util"
)

func main() {
	if !util.IsAdmin() {
		util.RunWithElevated()
	} else {
		_ = pkg.RootCmd.Execute()
	}
}
