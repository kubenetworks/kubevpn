package cmds

import (
	"context"
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

func CmdQuit(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quit",
		Short: i18n.T("Quit kubevpn daemon server"),
		Long:  templates.LongDesc(i18n.T(`Disconnect from cluster, leave proxy resources, and quit daemon`)),
		Example: templates.Examples(i18n.T(`
        # before quit kubevpn, it will leave proxy resources to origin and disconnect from cluster
        kubevpn quit
`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = quit(cmd.Context(), true)
			_ = quit(cmd.Context(), false)
			fmt.Fprint(os.Stdout, "quit successfully")
			return nil
		},
	}
	return cmd
}

func quit(ctx context.Context, isSudo bool) error {
	cli := daemon.GetClient(isSudo)
	if cli == nil {
		return nil
	}
	client, err := cli.Quit(ctx, &rpc.QuitRequest{})
	if err != nil {
		return err
	}
	var resp *rpc.QuitResponse
	for {
		resp, err = client.Recv()
		if err == io.EOF {
			break
		} else if err == nil {
			fmt.Fprint(os.Stdout, resp.Message)
		} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
			return nil
		} else {
			return err
		}
	}
	return nil
}
