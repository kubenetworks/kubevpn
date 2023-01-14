package cmds

import (
	"context"
	"net/http"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdServe(factory cmdutil.Factory) *cobra.Command {
	var route handler.Route
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Server side, startup traffic manager, forward inbound and outbound traffic",
		Long:  `Server side, startup traffic manager, forward inbound and outbound traffic.`,
		PreRun: func(*cobra.Command, []string) {
			util.InitLogger(config.Debug)
			go func() { log.Info(http.ListenAndServe("localhost:6060", nil)) }()
		},
		Run: func(cmd *cobra.Command, args []string) {
			err := handler.Start(context.Background(), route)
			if err != nil {
				log.Fatal(err)
			}
			select {}
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	cmd.Flags().StringVarP(&route.ChainNode, "chainCommand", "F", "", "command needs to be executed")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
