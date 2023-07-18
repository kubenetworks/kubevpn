package cmds

import (
	"context"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

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
			go util.StartupPProf(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			rand.Seed(time.Now().UnixNano())
			_, _ = maxprocs.Set(maxprocs.Logger(nil))
			err := handler.Complete(route)
			if err != nil {
				return err
			}
			defer handler.Final()
			ctx, cancelFunc := context.WithCancel(context.Background())
			stopChan := make(chan os.Signal)
			signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
			go func() {
				<-stopChan
				cancelFunc()
			}()
			servers, err := handler.Parse(*route)
			if err != nil {
				return err
			}
			return handler.Run(ctx, servers)
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "node", "L", []string{}, "Startup node server. eg: tcp://localhost:1080")
	cmd.Flags().StringVarP(&route.ChainNode, "chain", "F", "", "Forward chain. eg: tcp://192.168.1.100:2345")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug log or not")
	return cmd
}
