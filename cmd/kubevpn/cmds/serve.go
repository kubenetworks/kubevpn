package cmds

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
		RunE: func(cmd *cobra.Command, args []string) error {
			if v, ok := os.LookupEnv(config.EnvInboundPodTunIP); ok && v == "" {
				namespace := os.Getenv(config.EnvPodNamespace)
				if namespace == "" {
					return fmt.Errorf("can not get namespace")
				}
				url := fmt.Sprintf("%s.%s:80/rent/ip", config.ConfigMapPodTrafficManager, namespace)
				request, err := http.NewRequest("GET", url, nil)
				if err != nil {
					return fmt.Errorf("can not new request, err: %v", err)
				}
				request.Header.Set(config.HeaderPodName, os.Getenv(config.EnvPodName))
				request.Header.Set(config.HeaderPodNamespace, namespace)
				ip, err := util.DoReq(request)
				if err != nil {
					log.Error(err)
					return err
				}
				err = os.Setenv(config.EnvInboundPodTunIP, string(ip))
				if err != nil {
					log.Error(err)
					return err
				}
			}
			ctx, cancelFunc := context.WithCancel(context.Background())
			stopChan := make(chan os.Signal)
			signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
			go func() {
				<-stopChan
				cancelFunc()
			}()
			err := handler.Start(ctx, route)
			if err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		},
		PostRunE: func(cmd *cobra.Command, args []string) error {
			v, ok := os.LookupEnv(config.EnvInboundPodTunIP)
			if !ok || v == "" {
				return nil
			}
			_, _, err := net.ParseCIDR(v)
			if err != nil {
				return err
			}
			namespace := os.Getenv(config.EnvPodNamespace)
			url := fmt.Sprintf("%s.%s:80/release/ip", config.ConfigMapPodTrafficManager, namespace)
			request, err := http.NewRequest("DELETE", url, nil)
			if err != nil {
				return fmt.Errorf("can not new request, err: %v", err)
			}
			request.Header.Set(config.HeaderPodName, os.Getenv(config.EnvPodName))
			request.Header.Set(config.HeaderPodNamespace, namespace)
			request.Header.Set(config.HeaderIP, v)
			_, err = util.DoReq(request)
			return err
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	cmd.Flags().StringVarP(&route.ChainNode, "chainCommand", "F", "", "command needs to be executed")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
