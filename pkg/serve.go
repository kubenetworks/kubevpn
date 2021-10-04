package pkg

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wencaiwulue/kubevpn/util"
)

func init() {
	ServerCmd.Flags().StringArrayVarP(&nodeConfig.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	ServerCmd.Flags().StringVarP(&nodeConfig.ChainNodes, "chainCommand", "F", "", "command needs to be executed")
	ServerCmd.Flags().BoolVar(&util.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(ServerCmd)
}

var ServerCmd = &cobra.Command{
	Use:   "serve",
	Short: "serve",
	Long:  `serve`,
	Args: func(cmd *cobra.Command, args []string) error {
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		util.SetupLogger()
		if err := start(); err != nil {
			log.Fatal(err)
		}
		select {}
	},
}
