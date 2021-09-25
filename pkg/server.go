package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"kubevpn/core"
)

func init() {
	core.SetLogger(&core.LogLogger{})
	ServerCmd.Flags().StringArrayVarP(&nodeConfig.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	ServerCmd.Flags().StringVarP(&nodeConfig.ChainNodes, "chainCommand", "F", "", "command needs to be executed")
	ServerCmd.Flags().BoolVar(&core.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(ServerCmd)
}

var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "server",
	Long:  `server`,
	Args: func(cmd *cobra.Command, args []string) error {
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := start(); err != nil {
			log.Fatal(err)
		}
		select {}
	},
}
