package cmds

import (
	"context"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdUninstall(f cmdutil.Factory) *cobra.Command {
	var sshConf = &pkgssh.SshConfig{}
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall all resource create by kubevpn in k8s cluster",
		Long: templates.LongDesc(i18n.T(`
		Uninstall all resource create by kubevpn in k8s cluster
		
		Uninstall will delete all resources create by kubevpn in k8s cluster, like deployment, service, serviceAccount...
		and it will also delete local develop docker containers, docker networks. delete hosts entry added by kubevpn,
		cleanup DNS settings.
		`)),
		Example: templates.Examples(i18n.T(`
        # Uninstall default namespace
		  kubevpn uninstall

		# Uninstall another namespace test
		  kubevpn uninstall -n test

		# Uninstall cluster api-server behind of bastion host or ssh jump host
		kubevpn uninstall --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn uninstall --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn uninstall --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn uninstall --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn uninstall --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			disconnectResp, err := cli.Disconnect(context.Background())
			if err != nil {
				plog.G(cmd.Context()).Warnf("Failed to disconnect from cluter: %v", err)
			} else {
				err = disconnectResp.Send(&rpc.DisconnectRequest{
					KubeconfigBytes: ptr.To(string(bytes)),
					Namespace:       ptr.To(ns),
					SshJump:         sshConf.ToRPC(),
				})
				if err != nil {
					plog.G(cmd.Context()).Warnf("Failed to disconnect from cluter: %v", err)
				}
				_ = util.PrintGRPCStream[rpc.DisconnectResponse](cmd.Context(), disconnectResp)
			}

			req := &rpc.UninstallRequest{
				KubeconfigBytes: string(bytes),
				Namespace:       ns,
				SshJump:         sshConf.ToRPC(),
			}
			resp, err := cli.Uninstall(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.UninstallResponse](cmd.Context(), resp)
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
