package cmds

import (
	"fmt"
	"io"
	defaultlog "log"
	"net/http"
	"os"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
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

		# Connect to api-server behind of bastion host or ssh jump host
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile /Users/naison/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌─────┐      ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc ├─────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└─────┘      └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn connect --ssh-alias <alias>

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
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
			return handler.SshJump(sshConf, cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := connect.InitClient(f); err != nil {
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
	}
	cmd.Flags().StringArrayVar(&connect.Workloads, "workloads", []string{}, "Kubernetes workloads, special which workloads you want to proxy it to local PC, If not special, just connect to cluster network, like: pods/tomcat, deployment/nginx, replicaset/tomcat etc")
	cmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")

	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	cmd.Flags().StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	return cmd
}
