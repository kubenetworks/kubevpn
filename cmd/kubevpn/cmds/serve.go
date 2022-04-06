package cmds

import (
	"context"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	config2 "github.com/wencaiwulue/kubevpn/config"
	"github.com/wencaiwulue/kubevpn/pkg"
	"github.com/wencaiwulue/kubevpn/util"
	"net/http"
)

var config pkg.Route

func init() {
	ServerCmd.Flags().StringArrayVarP(&config.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	ServerCmd.Flags().StringVarP(&config.ChainNode, "chainCommand", "F", "", "command needs to be executed")
	ServerCmd.Flags().BoolVar(&config2.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(ServerCmd)
}

var ServerCmd = &cobra.Command{
	Use:   "serve",
	Short: "serve",
	Long:  `serve`,
	PreRun: func(*cobra.Command, []string) {
		util.InitLogger(config2.Debug)
		go func() { log.Info(http.ListenAndServe("localhost:6060", nil)) }()
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := pkg.Start(context.TODO(), config); err != nil {
			log.Fatal(err)
		}
		select {}
	},
}
