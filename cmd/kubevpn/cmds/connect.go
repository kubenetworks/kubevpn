package cmds

import (
	"fmt"
	"io"
	defaultlog "log"
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

func CmdConnect(f cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var sshConf = util.SshConfig{}
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
		PreRun: func(*cobra.Command, []string) {
			if !util.IsAdmin() {
				util.RunWithElevated()
				os.Exit(0)
			}
			go http.ListenAndServe("localhost:6060", nil)
			util.InitLogger(config.Debug)
			defaultlog.Default().SetOutput(io.Discard)
			if util.IsWindows() {
				driver.InstallWireGuardTunDriver()
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := connect.InitClient(f, cmd.Flags(), sshConf); err != nil {
				return err
			}
			connect.PreCheckResource()
			if err := connect.DoConnect(); err != nil {
				log.Errorln(err)
				handler.Cleanup(syscall.SIGQUIT)
			} else {
				fmt.Println()
				fmt.Println(`---------------------------------------------------------------------------`)
				fmt.Println(`    Now you can access resources in the kubernetes cluster, enjoy it :)    `)
				fmt.Println(`---------------------------------------------------------------------------`)
				fmt.Println()
			}
			select {}
		},
		PostRun: func(*cobra.Command, []string) {
			if util.IsWindows() {
				err := retry.OnError(retry.DefaultRetry, func(err error) bool {
					return err != nil
				}, func() error {
					return driver.UninstallWireGuardTunDriver()
				})
				if err != nil {
					var wd string
					wd, err = os.Getwd()
					if err != nil {
						return
					}
					filename := filepath.Join(wd, "wintun.dll")
					var temp *os.File
					if temp, err = os.CreateTemp("", ""); err != nil {
						return
					}
					if err = temp.Close(); err != nil {
						return
					}
					if err = os.Rename(filename, temp.Name()); err != nil {
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

	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "ssh connection string address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "username for ssh")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "password for ssh")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "file with private key for SSH authentication")
	return cmd
}
