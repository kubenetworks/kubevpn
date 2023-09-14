package cmds

import (
	"math/rand"
	"runtime"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.uber.org/automaxprocs/maxprocs"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdServe(_ cmdutil.Factory) *cobra.Command {
	var route = &core.Route{}
	cmd := &cobra.Command{
		Use:    "serve",
		Hidden: true,
		Short:  "Server side, startup traffic manager, forward inbound and outbound traffic",
		Long:   `Server side, startup traffic manager, forward inbound and outbound traffic.`,
		PreRun: func(*cobra.Command, []string) {
			util.InitLogger(config.Debug)
			runtime.GOMAXPROCS(0)
			go util.StartupPProf(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			rand.Seed(time.Now().UnixNano())
			_, _ = maxprocs.Set(maxprocs.Logger(nil))
			err := handler.RentIPIfNeeded(route)
			if err != nil {
				return err
			}
			defer func() {
				err := handler.ReleaseIPIfNeeded()
				if err != nil {
					log.Error(err)
				}
			}()
			servers, err := handler.Parse(*route)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			return handler.Run(ctx, servers)
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "node", "L", []string{}, "Startup node server. eg: tcp://localhost:1080")
	cmd.Flags().StringVarP(&route.ChainNode, "chain", "F", "", "Forward chain. eg: tcp://192.168.1.100:2345")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug log or not")
	return cmd
}
