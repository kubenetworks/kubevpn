package cmds

import (
	"fmt"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	utilcomp "k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdProxy(f cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: i18n.T("Proxy kubernetes workloads inbound traffic into local PC"),
		Long:  templates.LongDesc(i18n.T(`Proxy kubernetes workloads inbound traffic into local PC`)),
		Example: templates.Examples(i18n.T(`
		# Reverse proxy
		- proxy deployment
		  kubevpn proxy deployment/productpage

		- proxy service
		  kubevpn proxy service/productpage

        - proxy multiple workloads
          kubevpn proxy deployment/authors deployment/productpage
          or 
          kubevpn proxy deployment authors productpage

		# Reverse proxy with mesh, traffic with header a=1, will hit local PC, otherwise no effect
		kubevpn proxy service/productpage --headers a=1

		# Connect to api-server behind of bastion host or ssh jump host and proxy kubernetes resource traffic into local PC
		kubevpn proxy deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem --headers a=1

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn proxy service/productpage --ssh-alias <alias> --headers a=1

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if err = startupDaemon(cmd.Context()); err != nil {
				return err
			}
			// not support temporally
			if connect.Engine == config.EngineGvisor {
				return fmt.Errorf(`not support type engine: %s, support ("%s"|"%s")`, config.EngineGvisor, config.EngineMix, config.EngineRaw)
			}
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintf(os.Stdout, "You must specify the type of resource to proxy. %s\n\n", cmdutil.SuggestAPIResources("kubevpn"))
				fullCmdName := cmd.Parent().CommandPath()
				usageString := "Required resource not specified."
				if len(fullCmdName) > 0 && cmdutil.IsSiblingCommandExists(cmd, "explain") {
					usageString = fmt.Sprintf("%s\nUse \"%s explain <resource>\" for a detailed description of that resource (e.g. %[2]s explain pods).", usageString, fullCmdName)
				}
				return cmdutil.UsageErrorf(cmd, usageString)
			}

			bytes, err := util.ConvertToKubeconfigBytes(f)
			if err != nil {
				return err
			}
			// todo 将 doConnect 方法封装？内部使用 client 发送到daemon？
			client, err := daemon.GetClient(true).Connect(
				cmd.Context(),
				&rpc.ConnectRequest{
					KubeconfigBytes:  string(bytes),
					Namespace:        connect.Namespace,
					Headers:          connect.Headers,
					Workloads:        args,
					ExtraCIDR:        connect.ExtraCIDR,
					ExtraDomain:      connect.ExtraDomain,
					UseLocalDNS:      connect.UseLocalDNS,
					Engine:           string(connect.Engine),
					Addr:             sshConf.Addr,
					User:             sshConf.User,
					Password:         sshConf.Password,
					Keyfile:          sshConf.Keyfile,
					ConfigAlias:      sshConf.ConfigAlias,
					RemoteKubeconfig: sshConf.RemoteKubeconfig,
					TransferImage:    transferImage,
					Image:            config.Image,
					Level:            int32(log.DebugLevel),
				},
			)
			if err != nil {
				return err
			}
			var resp *rpc.ConnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					break
				} else if err == nil {
					log.Print(resp.Message)
				} else {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "Traffic with special headers with reverse it to local PC, you should startup your service after reverse workloads successfully, If not special, redirect all traffic to local PC, format is k=v, like: k1=v1,k2=v2")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "Use this image to startup container")
	cmd.Flags().StringArrayVar(&connect.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&connect.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().StringVar((*string)(&connect.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	addSshFlags(cmd, sshConf)
	cmd.ValidArgsFunction = utilcomp.ResourceTypeAndNameCompletionFunc(f)
	return cmd
}

func addSshFlags(cmd *cobra.Command, sshConf *util.SshConfig) {
	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	cmd.Flags().StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	cmd.Flags().StringVar(&sshConf.RemoteKubeconfig, "remote-kubeconfig", "", "Remote kubeconfig abstract path of ssh server, default is /$ssh-user/.kube/config")
	lookup := cmd.Flags().Lookup("remote-kubeconfig")
	lookup.NoOptDefVal = "~/.kube/config"
}
