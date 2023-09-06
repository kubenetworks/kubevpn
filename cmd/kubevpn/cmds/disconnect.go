package cmds

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdDisconnect(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "disconnect",
		Short:   i18n.T("Disconnect from kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Disconnect from kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if daemon.GetClient(false) == nil {
				return fmt.Errorf("daemon not start")
			}
			if daemon.GetClient(true) == nil {
				return fmt.Errorf("sudo daemon not start")
			}
			return
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(false).Disconnect(
				cmd.Context(),
				&rpc.DisconnectRequest{},
			)
			if err != nil {
				return err
			}
			var resp *rpc.DisconnectResponse
			for {
				resp, err = client.Recv()
				if err == io.EOF {
					return nil
				} else if err == nil {
					fmt.Print(resp.Message)
				} else {
					return err
				}
			}
		},
	}
	return cmd
}
