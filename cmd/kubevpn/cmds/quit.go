package cmds

import (
	"context"
	"fmt"
	"os"

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
			_ = quit(cmd.Context(), true)
			_ = quit(cmd.Context(), false)
			util.CleanExtensionLib()
			_, _ = fmt.Fprint(os.Stdout, "Exited")
			return nil
		},
	}
	return cmd
}

func quit(ctx context.Context, isSudo bool) error {
	cli, err := daemon.GetClient(isSudo)
	if err != nil {
		return err
	}
	client, err := cli.Quit(ctx, &rpc.QuitRequest{})
	if err != nil {
		return err
	}
	err = util.PrintGRPCStream[rpc.QuitResponse](client)
	if err != nil {
		if status.Code(err) == codes.Canceled {
			return nil
		}
		return err
	}
	return nil
}
