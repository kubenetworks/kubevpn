package cmds

import (
	"context"
	"fmt"
	"k8s.io/klog/v2"
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
				clientset, err := factory.KubernetesClientSet()
				if err != nil {
					klog.Error(err)
					return err
				}
				namespace, found, _ := factory.ToRawKubeConfigLoader().Namespace()
				if !found {
					namespace = os.Getenv(config.EnvNamespace)
				}
				if namespace == "" {
					return fmt.Errorf("can not get namespace")
				}
				cmi := clientset.CoreV1().ConfigMaps(namespace)
				dhcp := handler.NewDHCPManager(cmi, namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask})
				random, err := dhcp.RentIPRandom()
				if err != nil {
					klog.Error(err)
					return err
				}
				err = os.Setenv(config.EnvInboundPodTunIP, random.String())
				if err != nil {
					klog.Error(err)
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
			_, ipNet, err := net.ParseCIDR(v)
			if err != nil {
				return err
			}
			clientset, err := factory.KubernetesClientSet()
			if err != nil {
				return err
			}
			namespace, found, _ := factory.ToRawKubeConfigLoader().Namespace()
			if !found {
				namespace = os.Getenv(config.EnvNamespace)
			}
			if namespace == "" {
				return fmt.Errorf("can not get namespace")
			}
			cmi := clientset.CoreV1().ConfigMaps(namespace)
			dhcp := handler.NewDHCPManager(cmi, namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask})
			err = dhcp.ReleaseIpToDHCP(ipNet)
			return err
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "nodeCommand", "L", []string{}, "command needs to be executed")
	cmd.Flags().StringVarP(&route.ChainNode, "chainCommand", "F", "", "command needs to be executed")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
