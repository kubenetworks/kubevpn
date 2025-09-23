package cmds

import (
	"context"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdReset(f cmdutil.Factory) *cobra.Command {
	var sshConf = &pkgssh.SshConfig{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset workloads to origin spec",
		Long: templates.LongDesc(i18n.T(`
		Reset workloads to origin spec
		
		Reset will remove injected container envoy-proxy and vpn, and restore service mesh rules.
		`)),
		Example: templates.Examples(i18n.T(`
        # Reset default namespace workloads depooyment/productpage
		  kubevpn reset deployment/productpage

		# Reset another namespace test workloads depooyment/productpage
		  kubevpn reset deployment/productpage -n test

		# Reset workloads depooyment/productpage which api-server behind of bastion host or ssh jump host
		kubevpn reset deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn reset deployment/productpage --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn reset deployment/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn reset deployment/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn reset deployment/productpage --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		Args: cobra.MatchAll(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			req := &rpc.ResetRequest{
				KubeconfigBytes: string(bytes),
				Namespace:       ns,
				Workloads:       args,
				SshJump:         sshConf.ToRPC(),
			}
			resp, err := cli.Reset(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.ResetResponse](cmd.Context(), resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			return nil
		},
	}

	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}
