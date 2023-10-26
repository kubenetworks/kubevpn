package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdStatus(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: i18n.T("KubeVPN status"),
		Long:  templates.LongDesc(i18n.T(`KubeVPN status`)),
		Example: templates.Examples(i18n.T(`
        # show status for kubevpn status
        kubevpn status
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(false).Status(
				cmd.Context(),
				&rpc.StatusRequest{},
			)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, client.GetMessage())
			return nil
		},
	}
	return cmd
}
