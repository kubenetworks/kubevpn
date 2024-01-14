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

func CmdLeave(f cmdutil.Factory) *cobra.Command {
	var leaveCmd = &cobra.Command{
		Use:   "leave",
		Short: "Leave proxy resource",
		Long:  `leave proxy resource and restore it to origin`,
		Example: templates.Examples(i18n.T(`
		# leave proxy resource and restore it to origin
		kubevpn leave deployment/authors
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
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
				} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
					return nil
				} else if err != nil {
					return err
				}
				fmt.Fprint(os.Stdout, recv.GetMessage())
			}
		},
	}
	return leaveCmd
}
