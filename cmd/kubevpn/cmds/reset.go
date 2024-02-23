package cmds

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdReset(f cmdutil.Factory) *cobra.Command {
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
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeconfigBytes(f)
			if err != nil {
				return err
			}
			req := &rpc.ResetRequest{
				KubeconfigBytes: string(bytes),
				Namespace:       ns,
				SshJump:         sshConf.ToRPC(),
			}
			cli := daemon.GetClient(false)
			resp, err := cli.Reset(cmd.Context(), req)
			if err != nil {
				return err
			}
			for {
				recv, err := resp.Recv()
				if err == io.EOF {
					break
				} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
					return nil
				} else if err != nil {
					return err
				}
				fmt.Fprint(os.Stdout, recv.GetMessage())
			}
			return nil
		},
	}

	addSshFlags(cmd, sshConf)
	return cmd
}
