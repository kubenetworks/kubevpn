package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds"
	"net/http"
	_ "net/http/pprof"
)

func main() {
	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()
	_ = cmds.RootCmd.Execute()
}
