package cmds

import (
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdLeave(f cmdutil.Factory) *cobra.Command {
	var reverseStartCmd = &cobra.Command{
		Use:   "start",
		Short: "start reverse remote resource traffic to local machine",
		Long:  `reverse remote traffic to local machine`,
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			_, err = daemon.GetClient(true).Status(cmd.Context(), &rpc.StatusRequest{})
			if err != nil {
				if !util.IsAdmin() {
					util.RunWithElevated()
					os.Exit(0)
				}
				command := exec.Command("kubevpn", "daemon", "start")
				command.SysProcAttr = func() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }()
				err = command.Start()
				if err != nil {
					return
				}
				go command.Wait()
			}
			return
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	return reverseStartCmd
}
