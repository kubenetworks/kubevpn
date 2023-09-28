package cmds

import (
	"errors"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
)

func CmdDaemon(_ cmdutil.Factory) *cobra.Command {
	var opt = &daemon.SvrOption{}
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: i18n.T("Startup kubevpn daemon server"),
		Long:  i18n.T(`Startup kubevpn daemon server`),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			sockPath := daemon.GetSockPath(opt.IsSudo)
			err := os.Remove(sockPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			pidPath := daemon.GetPidPath(opt.IsSudo)
			err = os.Remove(pidPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			pid := os.Getpid()
			err = os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), os.ModePerm)
			if err != nil {
				return err
			}
			err = os.Chmod(pidPath, os.ModePerm)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			defer opt.Stop()
			return opt.Start(cmd.Context())
		},
		PostRun: func(cmd *cobra.Command, args []string) {
			sockPath := daemon.GetSockPath(opt.IsSudo)
			_ = os.Remove(sockPath)
			pidPath := daemon.GetPidPath(opt.IsSudo)
			_ = os.Remove(pidPath)
		},
		Hidden:                true,
		DisableFlagsInUseLine: true,
	}
	cmd.Flags().BoolVar(&opt.IsSudo, "sudo", false, "is sudo or not")
	return cmd
}
