package cmds

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdLeave(f cmdutil.Factory) *cobra.Command {
	var leaveCmd = &cobra.Command{
		Use:   "leave",
		Short: "leave reverse remote resource traffic to local machine",
		Long:  `leave remote traffic to local machine`,
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return startupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			leave, err := daemon.GetClient(false).Leave(cmd.Context(), &rpc.LeaveRequest{
				Workloads: args,
			})
			if err != nil {
				return err
			}
			for {
				recv, err := leave.Recv()
				if err == io.EOF {
					return nil
				} else if err != nil {
					return err
				}
				fmt.Print(recv.GetMessage())
			}
		},
	}
	return leaveCmd
}
