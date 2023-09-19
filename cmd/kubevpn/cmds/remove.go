package cmds

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdRemove(f cmdutil.Factory) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "remove",
		Short: "Remove reverse remote resource traffic to local machine",
		Long:  `Remove remote traffic to local machine`,
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
				fmt.Print(recv.GetMessage())
			}
		},
	}
	return cmd
}
