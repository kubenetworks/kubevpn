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
        kubevpn remove deployment/authors
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			leave, err := daemon.GetClient(false).Remove(cmd.Context(), &rpc.RemoveRequest{
				Workloads: args,
			})
			if err != nil {
				return err
			}
			for {
				recv, err := leave.Recv()
				if err == io.EOF {
					return nil
				} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
					return nil
				} else if err != nil {
					return err
				}
				fmt.Fprint(os.Stdout, recv.GetMessage())
			}
		},
	}
	return cmd
}
