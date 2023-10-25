package cmds

import (
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdConnectFork(f cmdutil.Factory) *cobra.Command {
	var connect = &handler.ConnectOptions{}
	var sshConf = &util.SshConfig{}
	var transferImage bool
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "connect-fork",
		Short:  i18n.T("Connect to kubernetes cluster network"),
		Long:   templates.LongDesc(i18n.T(`Connect to kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(`
		# Connect to k8s cluster network
		kubevpn connect

		# Connect to api-server behind of bastion host or ssh jump host
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile /Users/naison/.ssh/ssh.pem
		kubevpn connect --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn connect --ssh-alias <alias>

`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			util.InitLogger(false)
			if transferImage {
				err = util.TransferImage(cmd.Context(), sshConf, config.OriginImage, config.Image, os.Stdout)
				if err != nil {
					return err
				}
			}
			return handler.SshJumpAndSetEnv(cmd.Context(), sshConf, cmd.Flags(), true)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := connect.InitClient(f); err != nil {
				return err
			}
			_, err := connect.RentInnerIP(cmd.Context())
			if err != nil {
				return err
			}
			err = connect.DoConnect(cmd.Context())
			defer connect.Cleanup()
			if err != nil {
				log.Errorln(err)
			} else {
				util.Print(os.Stdout, "Now you can access resources in the kubernetes cluster, enjoy it :)")
			}
			<-cmd.Context().Done()
			return nil
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "enable debug mode or not, true or false")
	cmd.Flags().StringVar(&config.Image, "image", config.Image, "use this image to startup container")
	cmd.Flags().StringArrayVar(&connect.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringArrayVar(&connect.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	cmd.Flags().BoolVar(&transferImage, "transfer-image", false, "transfer image to remote registry, it will transfer image "+config.OriginImage+" to flags `--image` special image, default: "+config.Image)
	cmd.Flags().BoolVar(&connect.UseLocalDNS, "use-localdns", false, "if use-lcoaldns is true, kubevpn will start coredns listen at 53 to forward your dns queries. only support on linux now")
	cmd.Flags().StringVar((*string)(&connect.Engine), "engine", string(config.EngineRaw), fmt.Sprintf(`transport engine ("%s"|"%s") %s: use gvisor and raw both (both performance and stable), %s: use raw mode (best stable)`, config.EngineMix, config.EngineRaw, config.EngineMix, config.EngineRaw))

	addSshFlags(cmd, sshConf)
	return cmd
}
