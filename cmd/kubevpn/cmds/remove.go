package cmds

import (
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

func CmdRemove(f cmdutil.Factory) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "remove",
		Short: "Remove clone resource",
		Long: templates.LongDesc(i18n.T(`
		Remove clone resource

		This command is design to remove clone resources, after use command 'kubevpn clone xxx', 
		it will generate and create a new resource in target k8s cluster with format [resource_name]_clone_xxxxx,
		use this command to remove this created resources.
		`)),
		Example: templates.Examples(i18n.T(`
        # leave proxy resources to origin
        kubevpn remove deployment/authors-clone-645d7
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			resp, err := cli.Remove(cmd.Context(), &rpc.RemoveRequest{
				Workloads: args,
			})
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.RemoveResponse](resp)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			return nil
		},
	}
	return cmd
}
