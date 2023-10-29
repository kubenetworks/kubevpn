package cmds

import (
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/net/websocket"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// CmdSSH
// 设置本地的IP是223.254.0.1/32 ，记得一定是掩码 32位，
// 这样别的路由不会走到这里来
func CmdSSH(_ cmdutil.Factory) *cobra.Command {
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Ssh to jump server",
		Long:  `Ssh to jump server`,
		Example: templates.Examples(i18n.T(`
        # Jump to server behind of bastion host or ssh jump host
		kubevpn ssh --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────┘
		kubevpn ssh --ssh-alias <alias>
`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := websocket.NewConfig("ws://test/ws", "http://test")
			if err != nil {
				return err
			}
			config.Header.Set("ssh-addr", sshConf.Addr)
			config.Header.Set("ssh-username", sshConf.User)
			config.Header.Set("ssh-password", sshConf.Password)
			config.Header.Set("ssh-keyfile", sshConf.Keyfile)
			config.Header.Set("ssh-alias", sshConf.ConfigAlias)
			client := daemon.GetTCPClient(true)
			conn, err := websocket.NewClient(config, client)
			if err != nil {
				return err
			}
			go io.Copy(conn, os.Stdin)
			_, err = io.Copy(os.Stdout, conn)
			return err
		},
	}
	addSshFlags(cmd, sshConf)
	return cmd
}
