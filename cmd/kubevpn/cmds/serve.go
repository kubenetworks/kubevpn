package cmds

import (
	"context"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	_ "go.uber.org/automaxprocs"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdServe(factory cmdutil.Factory) *cobra.Command {
	var route = &core.Route{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Server side, startup traffic manager, forward inbound and outbound traffic",
		Long:  `Server side, startup traffic manager, forward inbound and outbound traffic.`,
		PreRun: func(*cobra.Command, []string) {
			util.InitLogger(config.Debug)
			go func() { log.Info(http.ListenAndServe("localhost:6060", nil)) }()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			rand.Seed(time.Now().UnixNano())
			err := handler.Complete(route)
			if err != nil {
				return err
			}
			ctx, cancelFunc := context.WithCancel(context.Background())
			stopChan := make(chan os.Signal)
			signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
			go func() {
				<-stopChan
				cancelFunc()
			}()
			err = handler.Start(ctx, *route)
			if err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
		PostRunE: func(cmd *cobra.Command, args []string) error { return handler.Final() },
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	cmd.Flags().StringVarP(&route.ChainNode, "chainCommand", "F", "", "command needs to be executed")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
