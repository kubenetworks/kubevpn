package cmds

import (
	"fmt"
	"os"

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
		Short: "Reset all changes made by KubeVPN",
		Long:  `Reset all changes made by KubeVPN`,
		Example: templates.Examples(i18n.T(`
        # Reset default namespace
		  kubevpn reset

		# Reset another namespace test
		  kubevpn reset -n test

		# Reset cluster api-server behind of bastion host or ssh jump host
		kubevpn reset --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn reset --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn reset --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn reset --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn reset --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return handler.SshJumpAndSetEnv(cmd.Context(), sshConf, cmd.Flags(), false)
		},
		Run: func(cmd *cobra.Command, args []string) {
			util.InitLogger(false)
			if err := connect.InitClient(factory); err != nil {
				log.Fatal(err)
			}
			_ = quit(cmd.Context(), true)
			_ = quit(cmd.Context(), false)
			err := connect.Reset(cmd.Context())
			if err != nil {
				log.Fatal(err)
			}
			fmt.Fprint(os.Stdout, "done")
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
