package cmds

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdReset(factory cmdutil.Factory) *cobra.Command {
	var connect = handler.ConnectOptions{}
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset KubeVPN",
		Long:  `Reset KubeVPN if any error occurs`,
		Example: templates.Examples(i18n.T(`
        # Reset default namespace
		  kubevpn reset

		# Reset another namespace test
		  kubevpn reset -n test

		# Reset cluster api-server behind of bastion host or ssh jump host
		kubevpn reset --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile /Users/naison/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn reset --ssh-alias <alias>

`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return handler.SshJump(sshConf, cmd.Flags())
		},
		Run: func(cmd *cobra.Command, args []string) {
			if err := connect.InitClient(factory); err != nil {
				log.Fatal(err)
			}
			err := connect.Reset(cmd.Context())
			if err != nil {
				log.Fatal(err)
			}
			log.Println("done")
		},
	}

	// for ssh jumper host
	cmd.Flags().StringVar(&sshConf.Addr, "ssh-addr", "", "Optional ssh jump server address to dial as <hostname>:<port>, eg: 127.0.0.1:22")
	cmd.Flags().StringVar(&sshConf.User, "ssh-username", "", "Optional username for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Password, "ssh-password", "", "Optional password for ssh jump server")
	cmd.Flags().StringVar(&sshConf.Keyfile, "ssh-keyfile", "", "Optional file with private key for SSH authentication")
	cmd.Flags().StringVar(&sshConf.ConfigAlias, "ssh-alias", "", "Optional config alias with ~/.ssh/config for SSH authentication")
	return cmd
}
