package cmds

import (
	"fmt"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func CmdList(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Short:   i18n.T("Disconnect from kubernetes cluster network"),
		Long:    templates.LongDesc(i18n.T(`Disconnect from kubernetes cluster network`)),
		Example: templates.Examples(i18n.T(``)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			if daemon.GetClient(true) == nil {
				return fmt.Errorf("sudo daemon not start")
			}
			return
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := daemon.GetClient(true).List(
				cmd.Context(),
				&rpc.ListRequest{},
			)
			if err != nil {
				return err
			}
			for _, workload := range client.Workloads {
				fmt.Println(workload)
			}
			return nil
		},
	}
	return cmd
}
