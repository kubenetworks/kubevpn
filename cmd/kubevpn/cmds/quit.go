package cmds

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdQuit(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "quit",
		Short:   i18n.T("Quit to kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Quit to kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if daemon.GetClient(false) == nil {
				fmt.Println("daemon not start")
			}
			if daemon.GetClient(true) == nil {
				fmt.Println("sudo daemon not start")
			}
			return
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = quit(cmd.Context(), true)
			_ = quit(cmd.Context(), false)
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
			fmt.Print(resp.Message)
		} else {
			return err
		}
	}
	return nil
}
