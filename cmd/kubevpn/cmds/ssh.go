package cmds

import (
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// CmdSSH
// 设置本地的IP是223.254.0.1/32 ，记得一定是掩码 32位，
// 这样别的路由不会走到这里来
func CmdSSH(_ cmdutil.Factory) *cobra.Command {
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:    "ssh",
		Hidden: true,
		Short:  "Ssh to jump server",
		Long:   `Ssh to jump server`,
		Example: templates.Examples(i18n.T(`
        # Jump to server behind of bastion host or ssh jump host
		kubevpn ssh --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────┘
		kubevpn ssh --ssh-alias <alias>
`)),
		PreRun: func(*cobra.Command, []string) {
			if !util.IsAdmin() {
				util.RunWithElevated()
				os.Exit(0)
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			err := handler.SSH(cmd.Context(), sshConf)
			return err
		},
	}
	addSshFlags(cmd, sshConf)
	return cmd
}
