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
// set local tun ip 198.19.0.1/32, remember to use mask 32
func CmdSSHDaemon(cmdutil.Factory) *cobra.Command {
	var clientIP string
	cmd := &cobra.Command{
		Use:    "ssh-daemon",
		Hidden: true,
		Short:  "Ssh daemon server",
		Long:   templates.LongDesc(i18n.T(`Ssh daemon server`)),
		Example: templates.Examples(i18n.T(`
        # SSH daemon server
        kubevpn ssh-daemon --client-ip 198.19.0.123/32
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
			_, err = fmt.Fprint(os.Stdout, client.ServerIP)
			return err
		},
	}
	cmd.Flags().StringVar(&clientIP, "client-ip", "", "Client cidr")
	return cmd
}
