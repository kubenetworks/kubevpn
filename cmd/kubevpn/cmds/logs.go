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
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdLogs(f cmdutil.Factory) *cobra.Command {
	req := &rpc.LogRequest{}
	cmd := &cobra.Command{
		Use:   "logs",
		Short: i18n.T("Log kubevpn daemon grpc server"),
		Long: templates.LongDesc(i18n.T(`
		Print the logs for kubevpn daemon grpc server. it will show sudo daemon and daemon grpc server log in both
		`)),
		Example: templates.Examples(i18n.T(`
        # show log for kubevpn daemon server
        kubevpn logs
        # follow more log
        kubevpn logs -f
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			plog.InitLoggerForClient()
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, err := daemon.GetClient(true)
			if err != nil {
				return err
			}
			client, err := cli.Logs(cmd.Context(), req)
			if err != nil {
				return err
			}
			err = util.PrintGRPCStream[rpc.LogResponse](client)
			if err != nil {
				if status.Code(err) == codes.Canceled {
					return nil
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&req.Follow, "follow", "f", false, "Specify if the logs should be streamed.")
	cmd.Flags().Int32VarP(&req.Lines, "number", "N", 10, "Lines of recent log file to display.")
	return cmd
}
