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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdLeave(f cmdutil.Factory) *cobra.Command {
	var leaveCmd = &cobra.Command{
		Use:   "leave",
		Short: i18n.T("Leave proxy resource"),
		Long: templates.LongDesc(i18n.T(`
		Leave proxy resource and restore it to origin
	
		This command is used to leave proxy resources. after use command 'kubevpn proxy xxx',
		you can use this command to leave proxy resources.
		you can just leave proxy resources which do proxy by yourself.
		and the last one leave proxy resource, it will also restore workloads container.
		otherwise it will keep containers [vpn, envoy-proxy] until last one to leave.
		`)),
		Example: templates.Examples(i18n.T(`
		# leave proxy resource and restore it to origin
		kubevpn leave deployment/authors
		`)),
		Args: cobra.MatchAll(cobra.OnlyValidArgs, cobra.MinimumNArgs(1)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			req := &rpc.LeaveRequest{
				Namespace: ns,
				Workloads: args,
			}
			resp, err := cli.Leave(context.Background())
			if err != nil {
				return err
			}
			err = resp.Send(req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.LeaveResponse](cmd.Context(), resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			return nil
		},
	}
	return leaveCmd
}
