package cmds

import (
	"fmt"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func CmdList(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: i18n.T("List proxy resources"),
		Long:  templates.LongDesc(i18n.T(`List proxy resources`)),
		Example: templates.Examples(i18n.T(`
	    # list proxy resources
        kubevpn list
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, err := daemon.GetClient(true)
			if err != nil {
				return err
			}
			client, err := cli.List(
				cmd.Context(),
				&rpc.ListRequest{},
			)
			if err != nil {
				return err
			}
			fmt.Println(client.GetMessage())
			return nil
		},
		Hidden: true,
	}
	return cmd
}
