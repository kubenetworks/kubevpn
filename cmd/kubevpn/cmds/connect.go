package cmds

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/driver"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdConnect(factory cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	cmd := &cobra.Command{
		Use:   "connect",
		Short: i18n.T("Connect to kubernetes cluster network, or proxy kubernetes workloads inbound traffic into local PC"),
		Long:  templates.LongDesc(i18n.T(`Connect to kubernetes cluster network, or proxy kubernetes workloads inbound traffic into local PC`)),
		Example: templates.Examples(i18n.T(`
		# Connect to k8s cluster network
		kubevpn connect

		# Reverse proxy
		- reverse deployment
		  kubevpn connect --workloads=deployment/productpage
		- reverse service
		  kubevpn connect --workloads=service/productpage

		# Reverse proxy with mesh, traffic with header a=1, will hit local PC, otherwise no effect
		kubevpn connect --workloads=service/productpage --headers a=1
`)),
		PersistentPreRun: func(*cobra.Command, []string) {
			if !util.IsAdmin() {
				util.RunWithElevated()
				os.Exit(0)
			} else {
				go func() { log.Info(http.ListenAndServe("localhost:6060", nil)) }()
			}
		},
		PreRun: func(*cobra.Command, []string) {
			util.InitLogger(config.Debug)
			if util.IsWindows() {
				driver.InstallWireGuardTunDriver()
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			if err := connect.InitClient(factory); err != nil {
				log.Fatal(err)
			}
			connect.PreCheckResource()
			if err := connect.DoConnect(); err != nil {
				log.Errorln(err)
				handler.Cleanup(syscall.SIGQUIT)
				select {}
			}
			fmt.Println(`---------------------------------------------------------------------------`)
			fmt.Println(`    Now you can access resources in the kubernetes cluster, enjoy it :)    `)
			fmt.Println(`---------------------------------------------------------------------------`)
			select {}
		},
		PostRun: func(_ *cobra.Command, _ []string) {
			if util.IsWindows() {
				if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
					return err != nil
				}, func() error {
					return driver.UninstallWireGuardTunDriver()
				}); err != nil {
					wd, _ := os.Getwd()
					filename := filepath.Join(wd, "wintun.dll")
					if err = os.Rename(filename, filepath.Join(os.TempDir(), "wintun.dll")); err != nil {
						log.Debugln(err)
					}
				}
			}
		},
	}
	cmd.Flags().StringArrayVar(&connect.Workloads, "workloads", []string{}, "Kubernetes workloads, special which workloads you want to proxy it to local PC, If not special, just connect to cluster network, like: pods/tomcat, deployment/nginx, replicaset/tomcat etc")
	cmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	return cmd
}
