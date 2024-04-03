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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdConfig(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Proxy kubeconfig which behind of ssh jump server",
	}
	cmd.AddCommand(cmdConfigAdd(f))
	cmd.AddCommand(cmdConfigRemove(f))
	return cmd
}

func cmdConfigAdd(f cmdutil.Factory) *cobra.Command {
	var sshConf = &util.SshConfig{}
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Proxy kubeconfig",
		Long:  templates.LongDesc(i18n.T(`proxy kubeconfig which behind of ssh jump server`)),
		Example: templates.Examples(i18n.T(`
		# proxy api-server which api-server behind of bastion host or ssh jump host
		kubevpn config add --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# it also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn config add --ssh-alias <alias>
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			req := &rpc.ConfigAddRequest{
				KubeconfigBytes: string(bytes),
				Namespace:       ns,
				SshJump:         sshConf.ToRPC(),
			}
			cli := daemon.GetClient(false)
			resp, err := cli.ConfigAdd(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, resp.ClusterID)
			return nil
		},
	}
	addSshFlags(cmd, sshConf)
	return cmd
}

func cmdConfigRemove(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove proxy kubeconfig",
		Long:  templates.LongDesc(i18n.T(`Remove proxy kubeconfig which behind of ssh jump server`)),
		Example: templates.Examples(i18n.T(`
		# remove proxy api-server which api-server behind of bastion host or ssh jump host
		kubevpn config remove --kubeconfig /var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/947048961.kubeconfig
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.OnlyValidArgs, cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &rpc.ConfigRemoveRequest{
				ClusterID: args[0],
			}
			cli := daemon.GetClient(false)
			_, err := cli.ConfigRemove(cmd.Context(), req)
			if err != nil {
				return err
			}
			return nil
		},
	}
	return cmd
}
