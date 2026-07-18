package cmds

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdQuit(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quit",
		Short: i18n.T("Quit kubevpn daemon grpc server"),
		Long: templates.LongDesc(i18n.T(`
		Disconnect from cluster, leave proxy resources, quit daemon grpc server and cleanup dns/hosts
		`)),
		Example: templates.Examples(i18n.T(`
        # before quit kubevpn, it will leave proxy resources to origin and disconnect from cluster
        kubevpn quit
		`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Quit the user daemon first: its connection cleanup runs server-side
			// leave-all, which reaches the traffic manager over the VPN and so needs the
			// data plane still up. The sudo daemon (which owns the TUN) is quit second.
			// Quitting sudo first would tear down the VPN before leave-all could run,
			// leaking proxy sidecars/rules until the manager's lease reaper reclaims them.
			_ = quit(cmd.Context(), false)
			_ = quit(cmd.Context(), true)
			util.CleanExtensionLib()
			printSuccess(os.Stdout, "Exited")
			return nil
		},
	}
	handler.AddDebugFlag(cmd.Flags())
	return cmd
}

func quit(ctx context.Context, isSudo bool) error {
	cli, err := daemon.GetClient(isSudo)
	if err != nil {
		return err
	}
	resp, err := cli.Quit(context.Background())
	if err != nil {
		return err
	}
	err = resp.Send(&rpc.QuitRequest{Level: plog.GetLogLevel()})
	if err != nil {
		return err
	}
	_, err = printProgressStream[rpc.QuitResponse](ctx, resp, os.Stdout)
	if err != nil {
		if status.Code(err) == codes.Canceled {
			return nil
		}
		return err
	}
	return nil
}
