package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// CmdSSHDaemon
// 设置本地的IP是223.254.0.1/32 ，记得一定是掩码 32位，
// 这样别的路由不会走到这里来
func CmdSSHDaemon(_ cmdutil.Factory) *cobra.Command {
	var clientIP string
	cmd := &cobra.Command{
		Use:    "ssh-daemon",
		Hidden: true,
		Short:  "Ssh daemon server",
		Long:   `Ssh daemon server`,
		Example: templates.Examples(i18n.T(`
        # SSH daemon server
        kubevpn ssh-daemon --client-ip 223.254.0.123/32
`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			err := daemon.StartupDaemon(cmd.Context())
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(true).SshStart(
				cmd.Context(),
				&rpc.SshStartRequest{
					ClientIP: clientIP,
				},
			)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, client.ServerIP)
			return nil
		},
	}
	cmd.Flags().StringVar(&clientIP, "client-ip", "", "Client cidr")
	return cmd
}
